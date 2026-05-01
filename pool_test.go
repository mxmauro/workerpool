package workerpool_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mxmauro/workerpool"
)

// -----------------------------------------------------------------------------

func TestPoolStopCancelsQueuedJobs(t *testing.T) {
	var executed [16]atomic.Bool

	var canceledMtx sync.Mutex
	canceledIDs := make([]string, 0, 2)

	pool := workerpool.New(workerpool.Options{
		Concurrency: 2,
		MaxCapacity: 16,
		OnCanceledJob: func(jobID string) {
			canceledMtx.Lock()
			canceledIDs = append(canceledIDs, jobID)
			canceledMtx.Unlock()
		},
	})

	startTime := time.Now()
	for i := 1; i <= 16; i++ {
		jobID := strconv.Itoa(i)
		if err := pool.QueueJobErr(jobID, func(ctx context.Context, workerNo int, jobID string) {
			jID, _ := strconv.Atoi(jobID)
			executed[jID-1].Store(true)

			toWait := 100 * time.Millisecond
			if jID == 13 || jID == 14 {
				// This is on purpose so job 15 and 16 are canceled.
				toWait = time.Second
			}
			dt := time.Since(startTime)

			t.Log("Running job", jobID, "at worker", "#"+strconv.Itoa(workerNo), "after", strconv.FormatInt(dt.Milliseconds(), 10)+"ms")
			select {
			case <-ctx.Done():
			case <-time.After(toWait):
			}
		}); err != nil {
			t.Fatalf("queue job %s: %v", jobID, err)
		}
	}

	time.Sleep(time.Second)

	pool.Stop()

	for i := 1; i <= 14; i++ {
		if !executed[i-1].Load() {
			t.Fatalf("job %d was not executed", i)
		}
	}
	for i := 15; i <= 16; i++ {
		if executed[i-1].Load() {
			t.Errorf("job %d was executed and should be canceled", i)
		}
	}

	canceledMtx.Lock()
	gotCanceled := append([]string(nil), canceledIDs...)
	canceledMtx.Unlock()

	slices.Sort(gotCanceled)
	if !reflect.DeepEqual(gotCanceled, []string{"15", "16"}) {
		t.Fatalf("unexpected canceled jobs: %v", gotCanceled)
	}
}

func TestQueueJobErrRejectsNilJob(t *testing.T) {
	pool := workerpool.New(workerpool.Options{})
	defer pool.Stop()

	if err := pool.QueueJobErr("job-1", nil); !errors.Is(err, workerpool.ErrNilJob) {
		t.Fatalf("expected ErrNilJob, got %v", err)
	}
}

func TestPoolReportsConfiguredState(t *testing.T) {
	pool := workerpool.New(workerpool.Options{
		Concurrency: 3,
		MaxCapacity: 5,
	})
	defer pool.Stop()

	if got := pool.Concurrency(); got != 3 {
		t.Fatalf("unexpected concurrency: %d", got)
	}
	if got := pool.MaxCapacity(); got != 5 {
		t.Fatalf("unexpected max capacity: %d", got)
	}
	if got := pool.PendingJobs(); got != 0 {
		t.Fatalf("unexpected pending jobs: %d", got)
	}
}

func TestPoolReportsPendingJobs(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	pool := workerpool.New(workerpool.Options{
		Concurrency: 1,
		MaxCapacity: 4,
	})
	defer pool.Stop()

	if err := pool.QueueJobErr("running", func(ctx context.Context, workerNo int, jobID string) {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("queue running job: %v", err)
	}

	<-started

	for i := 0; i < 3; i++ {
		jobID := "queued-" + strconv.Itoa(i)
		if err := pool.QueueJobErr(jobID, func(ctx context.Context, workerNo int, jobID string) {}); err != nil {
			t.Fatalf("queue %s: %v", jobID, err)
		}
	}

	if got := pool.PendingJobs(); got != 3 {
		t.Fatalf("unexpected pending jobs before release: %d", got)
	}

	close(release)

	deadline := time.Now().Add(time.Second)
	for {
		if pool.PendingJobs() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending jobs did not drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQueueJobErrReturnsQueueFull(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	pool := workerpool.New(workerpool.Options{
		Concurrency: 1,
		MaxCapacity: 1,
	})
	defer pool.Stop()

	if err := pool.QueueJobErr("running", func(ctx context.Context, workerNo int, jobID string) {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("queue running job: %v", err)
	}

	<-started

	if err := pool.QueueJobErr("queued", func(ctx context.Context, workerNo int, jobID string) {}); err != nil {
		t.Fatalf("queue queued job: %v", err)
	}

	if err := pool.QueueJobErr("overflow", func(ctx context.Context, workerNo int, jobID string) {}); !errors.Is(err, workerpool.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}

	close(release)
}

func TestQueueJobErrReturnsPoolStopped(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	pool := workerpool.New(workerpool.Options{
		Concurrency: 1,
	})

	if err := pool.QueueJobErr("running", func(ctx context.Context, workerNo int, jobID string) {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("queue running job: %v", err)
	}

	<-started

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- pool.StopContext(context.Background())
	}()

	time.Sleep(50 * time.Millisecond)

	if err := pool.QueueJobErr("late", func(ctx context.Context, workerNo int, jobID string) {}); !errors.Is(err, workerpool.ErrPoolStopped) {
		t.Fatalf("expected ErrPoolStopped, got %v", err)
	}

	close(release)

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("unexpected stop error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not complete")
	}
}

func TestStopContextTimesOut(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	pool := workerpool.New(workerpool.Options{
		Concurrency: 1,
	})

	if err := pool.QueueJobErr("blocking", func(ctx context.Context, workerNo int, jobID string) {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("queue job: %v", err)
	}

	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := pool.StopContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	select {
	case <-pool.Done():
		t.Fatal("pool shutdown completed before the running job was released")
	default:
	}

	close(release)

	select {
	case <-pool.Done():
	case <-time.After(time.Second):
		t.Fatal("pool shutdown did not complete")
	}
}

func TestOnJobPanicRecoversAndContinues(t *testing.T) {
	var recovered atomic.Int32
	executed := make(chan string, 1)

	pool := workerpool.New(workerpool.Options{
		Concurrency: 1,
		OnJobPanic: func(jobID string, recoveredValue any) {
			if jobID != "panic" {
				t.Errorf("unexpected panic job id: %s", jobID)
			}
			if recoveredValue != "boom" {
				t.Errorf("unexpected recovered value: %v", recoveredValue)
			}
			recovered.Add(1)
		},
	})
	defer pool.Stop()

	if err := pool.QueueJobErr("panic", func(ctx context.Context, workerNo int, jobID string) {
		panic("boom")
	}); err != nil {
		t.Fatalf("queue panic job: %v", err)
	}

	if err := pool.QueueJobErr("next", func(ctx context.Context, workerNo int, jobID string) {
		executed <- jobID
	}); err != nil {
		t.Fatalf("queue next job: %v", err)
	}

	select {
	case jobID := <-executed:
		if jobID != "next" {
			t.Fatalf("unexpected executed job id: %s", jobID)
		}
	case <-time.After(time.Second):
		t.Fatal("next job did not execute")
	}

	if recovered.Load() != 1 {
		t.Fatalf("expected one recovered panic, got %d", recovered.Load())
	}
}

func TestNegativeMaxCapacityMeansUnlimited(t *testing.T) {
	pool := workerpool.New(workerpool.Options{
		Concurrency: 1,
		MaxCapacity: -1,
	})
	defer pool.Stop()

	release := make(chan struct{})
	started := make(chan struct{})

	if err := pool.QueueJobErr("running", func(ctx context.Context, workerNo int, jobID string) {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("queue running job: %v", err)
	}

	<-started

	for i := 0; i < 8; i++ {
		jobID := "queued-" + strconv.Itoa(i)
		if err := pool.QueueJobErr(jobID, func(ctx context.Context, workerNo int, jobID string) {}); err != nil {
			t.Fatalf("queue %s: %v", jobID, err)
		}
	}

	close(release)
}
