package worker

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestProcessNormalOrderingAndBoundedConcurrency(t *testing.T) {
	jobs := make(chan int)
	go func() {
		defer close(jobs)
		for i := 1; i <= 32; i++ {
			jobs <- i
		}
	}()
	got, err := Process(context.Background(), jobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 32 {
		t.Fatalf("len=%d, want 32", len(got))
	}
	for i, value := range got {
		want := (i + 1) * (i + 1)
		if value != want {
			t.Fatalf("got[%d]=%d, want %d", i, value, want)
		}
	}
}

func TestProcessEmptyAndNilInput(t *testing.T) {
	closed := make(chan int)
	close(closed)
	got, err := Process(context.Background(), closed)
	if err != nil {
		t.Fatalf("closed empty error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("closed empty len = %d", len(got))
	}
	got, err = Process(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("nil input = %v, %v", got, err)
	}
}

func TestProcessPreCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	jobs := make(chan int)
	defer close(jobs)
	got, err := Process(ctx, jobs)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	if len(got) != 0 {
		t.Fatalf("pre-cancel results = %v", got)
	}
}

func TestProcessCancelWhileBlockedWaitingForInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := make(chan int)
	done := make(chan struct{})
	var got []int
	var err error
	go func() {
		got, err = Process(ctx, jobs)
		close(done)
	}()

	// Handshake: wait until Process is blocked on jobs or ctx by polling with cancel readiness.
	blocked := make(chan struct{})
	go func() {
		// Yield until the Process goroutine is likely waiting, then signal.
		for i := 0; i < 100; i++ {
			runtime.Gosched()
		}
		close(blocked)
	}()
	<-blocked
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not unblock on cancellation while waiting for input")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %v, want none accepted", got)
	}
}

func TestProcessCancelWhileProcessingDrainsAccepted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := make(chan int)
	done := make(chan struct{})
	var got []int
	var err error
	go func() {
		got, err = Process(ctx, jobs)
		close(done)
	}()

	const feed = 20
	for i := 1; i <= feed; i++ {
		select {
		case jobs <- i:
		case <-done:
			t.Fatalf("Process returned early after %d sends", i-1)
		case <-time.After(2 * time.Second):
			t.Fatal("unable to enqueue test jobs")
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not finish after cancel while processing")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	if len(got) == 0 {
		t.Fatal("accepted jobs were silently dropped on cancel")
	}
	for i, value := range got {
		want := (i + 1) * (i + 1)
		if value != want {
			t.Fatalf("accepted result[%d]=%d, want %d (full=%v)", i, value, want, got)
		}
	}
}

func TestProcessClosedInputCleanShutdown(t *testing.T) {
	jobs := make(chan int, 3)
	jobs <- 2
	jobs <- 3
	jobs <- 4
	close(jobs)
	got, err := Process(context.Background(), jobs)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{4, 9, 16}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestProcessRepeatedInvocationSafe(t *testing.T) {
	for round := 0; round < 5; round++ {
		jobs := make(chan int, 2)
		jobs <- 5
		jobs <- 6
		close(jobs)
		got, err := Process(context.Background(), jobs)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != 25 || got[1] != 36 {
			t.Fatalf("round %d got %v", round, got)
		}
	}
}

func TestProcessConcurrentCallsIndependent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			jobs := make(chan int, 1)
			jobs <- n
			close(jobs)
			got, err := Process(context.Background(), jobs)
			if err != nil {
				t.Errorf("n=%d err=%v", n, err)
				return
			}
			if len(got) != 1 || got[0] != n*n {
				t.Errorf("n=%d got=%v", n, got)
			}
		}(i + 1)
	}
	wg.Wait()
}

func TestProcessWorkersTerminate(t *testing.T) {
	before := runtime.NumGoroutine()
	jobs := make(chan int)
	go func() {
		defer close(jobs)
		for i := 0; i < 20; i++ {
			jobs <- i
		}
	}()
	if _, err := Process(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		runtime.Gosched()
	}
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestProcessAcceptedJobNotLostWhenCancelRacesEnqueue(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		ctx, cancel := context.WithCancel(context.Background())
		jobs := make(chan int, 8)
		for i := 1; i <= 8; i++ {
			jobs <- i
		}
		done := make(chan struct{})
		var got []int
		var err error
		go func() {
			got, err = Process(ctx, jobs)
			close(done)
		}()
		for i := 0; i < 5; i++ {
			runtime.Gosched()
		}
		cancel()
		close(jobs)
		<-done
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("trial %d err=%v", trial, err)
		}
		for i, value := range got {
			want := (i + 1) * (i + 1)
			if value != want {
				t.Fatalf("trial %d dropped/reordered accepted job: got=%v", trial, got)
			}
		}
	}
}
