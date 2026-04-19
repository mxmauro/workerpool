# workerpool

Yet another simple Go worker pool.

## Usage

```golang
package main

import (
	"context"
	"strconv"

	"github.com/mxmauro/workerpool"
)

func main() {
	pool := workerpool.New(workerpool.Options{
		Concurrency: 2,
		MaxCapacity: 16,
		OnCanceledJob: func(jobID string) {
			// This job was canceled
		},
		OnJobPanic: func(jobID string, recovered any) {
			// The job panicked and the worker was kept alive
		},
	})
	defer pool.Stop()

	for i := 1; i <= 16; i++ {
		err := pool.QueueJobErr(strconv.Itoa(i), func(ctx context.Context, workerNo int, jobID string) {
			// Job "i" is running
			// You can check for context cancellation and return earlier
		})
		if err != nil {
			// Queue rejected: pool stopping, queue full, or nil job routine
		}
	}

	// ... do some stuff ...
}
```

## Notes

- `QueueJob` preserves the original `bool` API for simple callers.
- `QueueJobErr` returns `ErrPoolStopped`, `ErrQueueFull`, or `ErrNilJob` for callers that need to distinguish failures.
- `Stop()` waits until running jobs return.
- `StopContext(ctx)` lets callers bound shutdown time if jobs do not return promptly.

## LICENSE

[MIT](/LICENSE)
