package workerpool_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/mxmauro/workerpool"
)

// -----------------------------------------------------------------------------

func TestPool(t *testing.T) {
	jobExecuted := make([]bool, 16)

	pool := workerpool.New(workerpool.Options{
		Concurrency: 2,
		MaxCapacity: 16,
		OnCanceledJob: func(jobID string) {
			t.Log("Job", jobID, "has been canceled")
		},
	})

	startTime := time.Now()
	for i := 1; i <= 16; i++ {
		jobID := strconv.Itoa(i)
		pool.QueueJob(jobID, func(ctx context.Context, workerNo int, jobID string) {
			jID, _ := strconv.Atoi(jobID)
			jobExecuted[jID-1] = true

			toWait := 100 * time.Millisecond
			if jID == 13 || jID == 14 {
				// This is on purpose so job 15 and 16 are canceled
				toWait = 1 * time.Second
			}
			dt := time.Since(startTime)

			t.Log("Running job", jobID, "at worker", "#"+strconv.Itoa(workerNo), "after", strconv.FormatInt(dt.Milliseconds(), 10)+"ms")
			select {
			case <-ctx.Done():
			case <-time.After(toWait):
			}

		})
	}

	time.Sleep(1 * time.Second)

	pool.Stop()

	// Extra check
	for i := 1; i <= 14; i++ {
		if !jobExecuted[i-1] {
			t.Fatal("Job", i, "was not executed")
		}
	}
	for i := 15; i <= 16; i++ {
		if jobExecuted[i-1] {
			t.Error("Job", i, "was executed and should be canceled")
		}
	}
}
