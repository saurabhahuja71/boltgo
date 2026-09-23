package agent

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func TestReadOnlyBatchPoolCloseDrainsAcceptedJobs(t *testing.T) {
	const jobs = 32
	var handled atomic.Int32
	p := newReadOnlyBatchPool(3, jobs, func(readOnlyBatchCall) { handled.Add(1) })
	for i := 0; i < jobs; i++ {
		if err := p.Submit(context.Background(), readOnlyBatchCall{}); err != nil {
			t.Fatal(err)
		}
	}
	p.Close()
	if got := handled.Load(); got != jobs {
		t.Fatalf("handled jobs = %d, want %d", got, jobs)
	}
}

func TestReadOnlyBatchPoolConcurrentSubmitAndClose(t *testing.T) {
	p := newReadOnlyBatchPool(3, 4, func(readOnlyBatchCall) {})
	var submitters sync.WaitGroup
	for i := 0; i < 32; i++ {
		submitters.Add(1)
		go func() {
			defer submitters.Done()
			_ = p.Submit(context.Background(), readOnlyBatchCall{})
		}()
	}
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	submitters.Wait()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Close did not finish")
	}
}

func TestReadOnlyBatchPoolCloseUnblocksFullQueueSubmit(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	p := newReadOnlyBatchPool(1, 1, func(readOnlyBatchCall) {
		startOnce.Do(func() { close(started) })
		<-release
	})
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- p.Submit(context.Background(), readOnlyBatchCall{}) }()
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case err := <-blocked:
		if !errors.Is(err, errReadOnlyBatchClosed) {
			t.Fatalf("blocked submit error = %v, want closed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock a full-queue submit")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not join workers after release")
	}
}

func TestReadOnlyBatchPoolBlockedSubmitHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	p := newReadOnlyBatchPool(1, 1, func(readOnlyBatchCall) {
		startOnce.Do(func() { close(started) })
		<-release
	})
	defer func() { close(release); p.Close() }()
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan error, 1)
	go func() { blocked <- p.Submit(ctx, readOnlyBatchCall{}) }()
	cancel()
	select {
	case err := <-blocked:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked submit error = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked submit did not honor cancellation")
	}
}

func TestReadOnlyBatchPoolSubmitAfterCloseAndConcurrentIdempotentClose(t *testing.T) {
	p := newReadOnlyBatchPool(2, 2, func(readOnlyBatchCall) {})
	p.Close()
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); !errors.Is(err, errReadOnlyBatchClosed) {
		t.Fatalf("submit after close error = %v, want closed", err)
	}
	var closers sync.WaitGroup
	for i := 0; i < 16; i++ {
		closers.Add(1)
		go func() { defer closers.Done(); p.Close() }()
	}
	closers.Wait()
	p.Close() // repeated Close must remain a no-op
}

func TestReadOnlyBatchPoolWorkersTerminateWithoutLeak(t *testing.T) {
	const workers = 4
	var alive atomic.Int32
	started := make(chan struct{}, workers)
	release := make(chan struct{})
	p := newReadOnlyBatchPool(workers, workers, func(readOnlyBatchCall) {
		alive.Add(1)
		started <- struct{}{}
		<-release
		alive.Add(-1)
	})
	for i := 0; i < workers; i++ {
		if err := p.Submit(context.Background(), readOnlyBatchCall{}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < workers; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	if got := alive.Load(); got != workers {
		t.Fatalf("alive workers = %d, want %d", got, workers)
	}
	close(release)
	p.Close()
	if got := alive.Load(); got != 0 {
		t.Fatalf("alive workers after Close = %d, want 0", got)
	}
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); !errors.Is(err, errReadOnlyBatchClosed) {
		t.Fatalf("submit after worker shutdown = %v, want closed", err)
	}
}

func TestReadOnlyBatchPoolBlockingJobDrainsBeforeCloseReturns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var startOnce sync.Once
	p := newReadOnlyBatchPool(1, 1, func(readOnlyBatchCall) {
		startOnce.Do(func() { close(started) })
		<-release
		close(finished)
	})
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); err != nil {
		t.Fatal(err)
	}
	<-started
	closeRequested := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		close(closeRequested)
		p.Close()
		close(closed)
	}()
	<-closeRequested
	select {
	case <-closed:
		t.Fatal("Close returned before the accepted blocking job finished")
	default:
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking job did not finish")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the accepted job drained")
	}
}

func TestReadOnlyBatchPoolCancelledContextDoesNotAcceptReadyQueue(t *testing.T) {
	p := newReadOnlyBatchPool(1, 8, func(readOnlyBatchCall) {})
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 64; i++ {
		if err := p.Submit(ctx, readOnlyBatchCall{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("submit with cancelled context = %v, want context.Canceled", err)
		}
	}
}

func TestReadOnlyBatchPoolNoSendOnClosedChannelPanic(t *testing.T) {
	started := make(chan struct{}, 64)
	release := make(chan struct{})
	p := newReadOnlyBatchPool(2, 1, func(readOnlyBatchCall) {
		started <- struct{}{}
		<-release
	})
	var submitters sync.WaitGroup
	for i := 0; i < 64; i++ {
		submitters.Add(1)
		go func() {
			defer submitters.Done()
			_ = p.Submit(context.Background(), readOnlyBatchCall{})
		}()
	}
	var closers sync.WaitGroup
	for i := 0; i < 8; i++ {
		closers.Add(1)
		go func() {
			defer closers.Done()
			p.Close()
		}()
	}
	close(release)
	submitters.Wait()
	closers.Wait()
	if err := p.Submit(context.Background(), readOnlyBatchCall{}); !errors.Is(err, errReadOnlyBatchClosed) {
		t.Fatalf("submit after concurrent close = %v, want closed", err)
	}
}

func TestExecuteReadOnlyBatchIsConcurrentAndBounded(t *testing.T) {
	const calls = 8
	started := make(chan struct{}, calls)
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	batch := make([]readOnlyBatchCall, calls)
	for i := range batch {
		index := i
		batch[i] = readOnlyBatchCall{index: index, run: func(context.Context) tools.ExecutionResult {
			current := active.Add(1)
			for {
				old := maxActive.Load()
				if current <= old || maxActive.CompareAndSwap(old, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return tools.ExecutionResult{Category: tools.FailureSuccess, Output: string(rune('a' + index))}
		}}
	}

	done := make(chan []tools.ExecutionResult, 1)
	go func() {
		results, err := executeReadOnlyBatch(context.Background(), batch)
		if err != nil {
			t.Errorf("executeReadOnlyBatch error = %v", err)
		}
		done <- results
	}()
	for i := 0; i < readOnlyBatchWorkers; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("batch did not start the bounded workers")
		}
	}
	if got := maxActive.Load(); got != readOnlyBatchWorkers {
		t.Fatalf("max concurrent workers = %d, want %d", got, readOnlyBatchWorkers)
	}
	close(release)
	results := <-done
	if len(results) != calls {
		t.Fatalf("result count = %d, want %d", len(results), calls)
	}
	for i, result := range results {
		want := string(rune('a' + i))
		if result.Output != want {
			t.Fatalf("result[%d] = %q, want %q", i, result.Output, want)
		}
	}
}

func TestExecuteReadOnlyBatchCancellationJoinsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, readOnlyBatchWorkers)
	finished := make(chan struct{}, readOnlyBatchWorkers)
	calls := make([]readOnlyBatchCall, readOnlyBatchWorkers)
	for i := range calls {
		calls[i] = readOnlyBatchCall{index: i, run: func(runCtx context.Context) tools.ExecutionResult {
			started <- struct{}{}
			<-runCtx.Done()
			finished <- struct{}{}
			return tools.ExecutionResult{Category: tools.FailureTimeout, Output: runCtx.Err().Error()}
		}}
	}
	done := make(chan error, 1)
	go func() {
		_, err := executeReadOnlyBatch(ctx, calls)
		done <- err
	}()
	for range calls {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("batch error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled batch did not return")
	}
	for range calls {
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not terminate after cancellation")
		}
	}
}

func TestExecuteReadOnlyBatchPreservesIndependentErrors(t *testing.T) {
	calls := []readOnlyBatchCall{
		{index: 0, run: func(context.Context) tools.ExecutionResult {
			return tools.ExecutionResult{Category: tools.FailureSuccess, Output: "first"}
		}},
		{index: 1, run: func(context.Context) tools.ExecutionResult {
			return tools.ExecutionResult{Category: tools.FailureNotFound, Output: "missing"}
		}},
		{index: 2, run: func(context.Context) tools.ExecutionResult {
			return tools.ExecutionResult{Category: tools.FailureSuccess, Output: "third"}
		}},
	}
	results, err := executeReadOnlyBatch(context.Background(), calls)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Output != "first" || results[1].Category != tools.FailureNotFound || results[2].Output != "third" {
		t.Fatalf("batch results were corrupted by one failure: %+v", results)
	}
}

func TestExecuteReadOnlyBatchEmptyInputDoesNotStartWorkers(t *testing.T) {
	results, err := executeReadOnlyBatch(context.Background(), nil)
	if err != nil || len(results) != 0 {
		t.Fatalf("empty batch = results %v, error %v", results, err)
	}
}

func TestReadOnlyBatchPrefixStopsAtStatefulCall(t *testing.T) {
	reg := tools.DefaultBuiltins(false)
	calls := []llm.ToolCall{
		{Function: llm.FunctionCall{Name: "read_file"}},
		{Function: llm.FunctionCall{Name: "write_file"}},
		{Function: llm.FunctionCall{Name: "read_file"}},
	}
	if got := readOnlyBatchPrefix(reg, calls); got != 1 {
		t.Fatalf("read-before-write prefix = %d, want 1", got)
	}
	calls[0].Function.Name = "write_file"
	if got := readOnlyBatchPrefix(reg, calls); got != 0 {
		t.Fatalf("write-before-read prefix = %d, want 0", got)
	}
}

func TestAgentPreservesBatchedReadHistoryOrder(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(workspace+"/"+name, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := tools.DefaultBuiltinsOpts(tools.BuiltinOpts{Workspace: workspace})
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSEMultiple(
				llm.ToolCall{ID: "a", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
				llm.ToolCall{ID: "b", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"b.txt"}`}},
				llm.ToolCall{ID: "c", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"c.txt"}`}},
			)
		}
		return textSSE("done")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "read these files", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, message := range ag.History {
		if message.Role == llm.RoleTool {
			got = append(got, message.ToolCallID)
		}
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool history order = %v, want %v", got, want)
	}
}
