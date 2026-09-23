package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

const readOnlyBatchWorkers = 3

var errReadOnlyBatchClosed = errors.New("read-only batch pool is closed")

type readOnlyBatchCall struct {
	index int
	run   func(context.Context) tools.ExecutionResult
}

// readOnlyBatchPool owns the jobs channel for its entire lifetime. Submitters
// never close it; Close first prevents new submissions, wakes blocked
// submitters, waits for submissions already in flight, and only then closes
// the channel so accepted jobs are drained by every worker.
type readOnlyBatchPool struct {
	jobs chan readOnlyBatchCall
	done chan struct{}

	mu     sync.Mutex
	closed bool

	activeSubmitters sync.WaitGroup
	workers          sync.WaitGroup
	closeOnce        sync.Once
}

func newReadOnlyBatchPool(workers, queueSize int, run func(readOnlyBatchCall)) *readOnlyBatchPool {
	if workers < 1 {
		workers = 1
	}
	if queueSize < 1 {
		queueSize = 1
	}
	p := &readOnlyBatchPool{
		jobs: make(chan readOnlyBatchCall, queueSize),
		done: make(chan struct{}),
	}
	p.workers.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer p.workers.Done()
			for call := range p.jobs {
				run(call)
			}
		}()
	}
	return p
}

func (p *readOnlyBatchPool) Submit(ctx context.Context, call readOnlyBatchCall) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errReadOnlyBatchClosed
	}
	p.activeSubmitters.Add(1)
	p.mu.Unlock()
	defer p.activeSubmitters.Done()

	// Prefer an already-cancelled context or shutdown signal over accepting
	// work when the jobs channel also happens to be writable. Without this
	// non-blocking check, a fair select can randomly enqueue after cancel.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errReadOnlyBatchClosed
	default:
	}

	select {
	case p.jobs <- call:
		return nil
	case <-p.done:
		return errReadOnlyBatchClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *readOnlyBatchPool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.done)
		p.mu.Unlock()

		// No submitter can be added after closed is set. Once these finish,
		// closing jobs is safe and all accepted calls are drained by workers.
		p.activeSubmitters.Wait()
		close(p.jobs)
		p.workers.Wait()
	})
}

// executeReadOnlyBatch runs one contiguous set of already-authorized,
// independently safe reads. The caller applies results and all agent state
// changes in index order after this returns.
func executeReadOnlyBatch(ctx context.Context, calls []readOnlyBatchCall) ([]tools.ExecutionResult, error) {
	results := make([]tools.ExecutionResult, len(calls))
	if len(calls) == 0 {
		return results, nil
	}

	workers := readOnlyBatchWorkers
	if len(calls) < workers {
		workers = len(calls)
	}
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := newReadOnlyBatchPool(workers, len(calls), func(call readOnlyBatchCall) {
		results[call.index] = call.run(batchCtx)
	})
	for _, call := range calls {
		if err := p.Submit(batchCtx, call); err != nil {
			p.Close()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, err
		}
	}
	p.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func readOnlyBatchPrefix(reg *tools.Registry, calls []llm.ToolCall) int {
	for i, call := range calls {
		if !reg.ReadOnlyBatchEligible(call.Function.Name) {
			return i
		}
	}
	return len(calls)
}
