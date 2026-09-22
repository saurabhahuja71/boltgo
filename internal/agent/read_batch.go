package agent

import (
	"context"
	"sync"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

const readOnlyBatchWorkers = 3

type readOnlyBatchCall struct {
	index int
	run   func(context.Context) tools.ExecutionResult
}

// executeReadOnlyBatch runs one contiguous set of already-authorized,
// independently safe reads. The caller applies results and all agent state
// changes in index order after this returns.
func executeReadOnlyBatch(ctx context.Context, calls []readOnlyBatchCall) ([]tools.ExecutionResult, error) {
	results := make([]tools.ExecutionResult, len(calls))
	if len(calls) == 0 {
		return results, nil
	}

	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan readOnlyBatchCall, len(calls))
	workers := readOnlyBatchWorkers
	if len(calls) < workers {
		workers = len(calls)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-batchCtx.Done():
					return
				case call, ok := <-jobs:
					if !ok {
						return
					}
					results[call.index] = call.run(batchCtx)
				}
			}
		}()
	}
	for _, call := range calls {
		select {
		case <-batchCtx.Done():
			close(jobs)
			wg.Wait()
			return nil, batchCtx.Err()
		case jobs <- call:
		}
	}
	close(jobs)
	wg.Wait()
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
