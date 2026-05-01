package workerpool

import (
	"container/list"
	"context"
	"errors"
	"runtime"
	"sync"

	"github.com/mxmauro/go-rundownprotection"
	"github.com/mxmauro/resetevent"
)

// -----------------------------------------------------------------------------

// Options specifies the pool characteristics.
type Options struct {
	// Concurrency is the maximum amount of simultaneous jobs to run. If zero, defaults to runtime.GOMAXPROCS(0).
	Concurrency int

	// MaxCapacity defines the maximum amount of jobs that can be queued. Zero or less for infinite.
	MaxCapacity int

	// OnCanceledJob is a function to call if a queued job is not executed due to the pool being stopped.
	OnCanceledJob CanceledJobCallback

	// OnJobPanic is a function to call if a job panics. If nil, the panic is rethrown.
	OnJobPanic JobPanicCallback
}

// Pool defines a worker pool job queue and scheduler.
type Pool struct {
	_ noCopy

	rp           *rundownprotection.RundownProtection
	wg           sync.WaitGroup
	shutdownSync sync.Once
	shutdownDone chan struct{}

	maxCapacity   int
	onCanceledJob CanceledJobCallback
	onJobPanic    JobPanicCallback

	jobsListMtx     sync.Mutex
	jobsList        *list.List
	jobsAvailableEv *resetevent.AutoResetEvent

	idleQueue      chan int
	workerJobQueue []chan Job
}

// Job defines a task to run inside the worker pool.
type Job struct {
	id string
	fn JobRoutine
}

// JobRoutine defines the callback function to run when the job is ready for execution. workerNo starts from 1.
type JobRoutine func(ctx context.Context, workerNo int, jobID string)

// CanceledJobCallback defines the function to call if a queued job is not executed due to the pool being stopped.
type CanceledJobCallback func(jobID string)

// JobPanicCallback defines the function to call if a queued job panics.
type JobPanicCallback func(jobID string, recovered any)

// -----------------------------------------------------------------------------

// noCopy helps go vet detect accidental copies of synchronization state.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// -----------------------------------------------------------------------------

var (
	// ErrPoolStopped is returned when a job is queued while the pool is stopping.
	ErrPoolStopped = errors.New("workerpool: pool is stopping")

	// ErrQueueFull is returned when a job cannot be queued because the pending queue reached MaxCapacity.
	ErrQueueFull = errors.New("workerpool: queue is full")

	// ErrNilJob is returned when a nil job routine is submitted.
	ErrNilJob = errors.New("workerpool: job routine is nil")
)

// -----------------------------------------------------------------------------

// New creates a worker pool and starts its workers and scheduler.
func New(opts Options) *Pool {
	// Validate and sanitize options
	if opts.Concurrency < 1 {
		opts.Concurrency = runtime.GOMAXPROCS(0)
	}
	if opts.MaxCapacity < 0 {
		opts.MaxCapacity = 0
	}

	// Create pool
	p := Pool{
		rp:           rundownprotection.Create(),
		wg:           sync.WaitGroup{},
		shutdownSync: sync.Once{},
		shutdownDone: make(chan struct{}),

		maxCapacity:   opts.MaxCapacity,
		onCanceledJob: opts.OnCanceledJob,
		onJobPanic:    opts.OnJobPanic,

		jobsListMtx:     sync.Mutex{},
		jobsList:        list.New(),
		jobsAvailableEv: resetevent.NewAutoResetEvent(),

		idleQueue:      make(chan int, opts.Concurrency),
		workerJobQueue: make([]chan Job, opts.Concurrency),
	}

	// Launch workers and the scheduler
	p.wg.Add(opts.Concurrency + 1)
	for i := 0; i < opts.Concurrency; i++ {
		p.workerJobQueue[i] = make(chan Job)
		p.idleQueue <- i
		go p.worker(i)
	}
	go p.scheduler()

	// Done
	return &p
}

// Stop starts pool shutdown and waits until it completes.
func (p *Pool) Stop() {
	_ = p.StopContext(context.Background())
}

// StopContext starts pool shutdown and waits until it completes or ctx is done.
func (p *Pool) StopContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.shutdownSync.Do(func() {
		go p.shutdown()
	})

	select {
	case <-p.shutdownDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done returns a channel that is closed when pool shutdown fully completes.
func (p *Pool) Done() <-chan struct{} {
	return p.shutdownDone
}

// Concurrency returns the configured amount of workers.
func (p *Pool) Concurrency() int {
	return len(p.workerJobQueue)
}

// MaxCapacity returns the configured queue capacity. Zero means unlimited.
func (p *Pool) MaxCapacity() int {
	return p.maxCapacity
}

// PendingJobs returns the amount of jobs waiting in the queue.
func (p *Pool) PendingJobs() int {
	p.jobsListMtx.Lock()
	defer p.jobsListMtx.Unlock()

	return p.jobsList.Len()
}

// QueueJob adds a job to the pool and returns false if it is rejected.
func (p *Pool) QueueJob(id string, fn JobRoutine) bool {
	return p.QueueJobErr(id, fn) == nil
}

// QueueJobErr adds a job to the pool and returns a typed error if it is rejected.
func (p *Pool) QueueJobErr(id string, fn JobRoutine) error {
	if fn == nil {
		return ErrNilJob
	}
	if !p.rp.Acquire() {
		return ErrPoolStopped
	}
	defer p.rp.Release()

	// Add to jobs list
	p.jobsListMtx.Lock()
	if p.maxCapacity > 0 && p.jobsList.Len() >= p.maxCapacity {
		p.jobsListMtx.Unlock()
		return ErrQueueFull
	}
	p.jobsList.PushBack(Job{
		id: id,
		fn: fn,
	})
	p.jobsListMtx.Unlock()
	p.jobsAvailableEv.Set()

	// Done
	return nil
}

func (p *Pool) scheduler() {
	defer p.wg.Done()

	for {
		select {
		case <-p.rp.Done():
			return

			// Wait until we have a job in the list
		case <-p.jobsAvailableEv.WaitCh():
			// Iterate until no more jobs
			for {
				select {
				case <-p.rp.Done():
					return
				default:
				}

				// Get next job
				job, ok := p.popJob()
				if !ok {
					break // End the loop if no more jobs
				}

				// Wait until we have an idle worker
				select {
				case <-p.rp.Done():
					// Pool is shutting down, put this job back on the queue so cancellation routine is called on it.
					p.jobsListMtx.Lock()
					p.jobsList.PushBack(job)
					p.jobsListMtx.Unlock()
					return

				case idleWorkerID := <-p.idleQueue:
					p.workerJobQueue[idleWorkerID] <- job
				}
			}
		}
	}
}

func (p *Pool) worker(workerID int) {
	defer p.wg.Done()

	for {
		select {
		case <-p.rp.Done():
			return

		case job, ok := <-p.workerJobQueue[workerID]:
			if !ok {
				return
			}
			p.runJob(workerID, job)

			// Queue this worker as idle if we are still running
			if p.rp.Acquire() {
				p.idleQueue <- workerID
				p.rp.Release()
			}
		}
	}
}

func (p *Pool) popJob() (Job, bool) {
	p.jobsListMtx.Lock()
	defer p.jobsListMtx.Unlock()

	return p.popJobNoLock()
}

func (p *Pool) popJobNoLock() (Job, bool) {
	elem := p.jobsList.Front()
	if elem == nil {
		return Job{}, false
	}
	p.jobsList.Remove(elem)

	// Get a job
	job := elem.Value.(Job)
	return job, true
}

func (p *Pool) runJob(workerID int, job Job) {
	if p.onJobPanic == nil {
		job.fn(p.rp, workerID+1, job.id)
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			p.onJobPanic(job.id, recovered)
		}
	}()

	job.fn(p.rp, workerID+1, job.id)
}

func (p *Pool) shutdown() {
	defer close(p.shutdownDone)

	// Initiate shutdown
	p.rp.Wait()
	p.wg.Wait()

	// Cancel pending jobs on the main queue.
	for {
		job, ok := p.popJobNoLock() // No need to lock because no other routine accesses it now.
		if !ok {
			break
		}
		if p.onCanceledJob != nil {
			p.onCanceledJob(job.id)
		}
	}

	// Misc cleanup
	close(p.idleQueue)
	for _, ch := range p.workerJobQueue {
		close(ch)
	}
}
