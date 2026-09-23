// Package worker provides bounded, ordered job processors.
package worker

import (
	"context"
	"sync"
)

// DefaultWorkers is the fixed concurrency bound for Process.
const DefaultWorkers = 3

// mapJob is the deterministic per-job transformation.
func mapJob(v int) int { return v * v }

// Process reads jobs from the input channel, transforms each value with a
// bounded worker pool, and returns outputs in input order.
//
// Cancellation / accepted-job contract:
//   - A job is accepted only after it has been received from jobs.
//   - On ctx cancellation, Process stops accepting further jobs, finishes every
//     already-accepted job, returns those results in order, and returns ctx.Err().
//   - Cancellation while blocked on a receive from jobs unblocks immediately
//     without accepting a new job.
//   - Closing jobs causes a clean drain of accepted work and a nil error.
//   - An empty/already-closed jobs channel returns a nil slice and a nil error.
//   - A pre-cancelled ctx returns nil, ctx.Err() without starting workers.
//
// Process keeps no package-level state; repeated calls are independent and safe.
func Process(ctx context.Context, jobs <-chan int) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if jobs == nil {
		return nil, nil
	}

	workers := DefaultWorkers
	work := make(chan indexed, workers)
	results := make(chan indexed, workers)

	var workerWG sync.WaitGroup
	workerWG.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer workerWG.Done()
			for job := range work {
				results <- indexed{index: job.index, value: mapJob(job.value)}
			}
		}()
	}

	var collectWG sync.WaitGroup
	byIndex := make(map[int]int)
	var mu sync.Mutex
	collectWG.Add(1)
	go func() {
		defer collectWG.Done()
		for result := range results {
			mu.Lock()
			byIndex[result.index] = result.value
			mu.Unlock()
		}
	}()

	go func() {
		workerWG.Wait()
		close(results)
	}()

	accepted := 0
	var runErr error
receive:
	for {
		select {
		case <-ctx.Done():
			runErr = ctx.Err()
			break receive
		case value, ok := <-jobs:
			if !ok {
				break receive
			}
			job := indexed{index: accepted, value: value}
			select {
			case work <- job:
				accepted++
			case <-ctx.Done():
				// Value was already received from jobs, so it is accepted.
				// Deliver it to a worker before shutting down the pool.
				runErr = ctx.Err()
				work <- job
				accepted++
				break receive
			}
		}
	}
	close(work)
	collectWG.Wait()

	out := make([]int, accepted)
	for i := 0; i < accepted; i++ {
		out[i] = byIndex[i]
	}
	if runErr != nil {
		return out, runErr
	}
	return out, nil
}

type indexed struct {
	index int
	value int
}
