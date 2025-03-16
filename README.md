# workerpool

Yet another simple Go worker pool.

## Usage

```golang
package main

import (
    "context"

    "github.com/mxmauro/workerpool"
)

func main() {
	pool := workerpool.New(workerpool.Options{
		Concurrency: 2,
		MaxCapacity: 16,
		OnCanceledJob: func(jobID string) {
			// This job was canceled
		},
	})
	defer pool.Stop()

	for i := 1; i <= 16; i++ {
		pool.QueueJob(strconv.Itoa(i), func(ctx context.Context, jobID string) {
			// Job "i" is running
			// You can check for context cancelation and return earlier

		})
	}

	// ... do some stuff ...
}
```

## LICENSE

[MIT](/LICENSE)
