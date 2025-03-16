package workerpool

import (
	"container/list"
	"context"
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

	// MaxCapacity defines the maximum amount of jobs that can be queued. Zero for infinite.
	MaxCapacity int

	// OnCanceledJob is a function to call if a queued job is not executed due to the pool being stopped.
	OnCanceledJob CanceledJobCallback
}

// Pool defines a worker pool job queue and scheduler.
type Pool struct {
	rp           *rundownprotection.RundownProtection
	wg           sync.WaitGroup
	shutdownSync sync.Once

	maxCapacity   int
	onCanceledJob CanceledJobCallback

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

// -----------------------------------------------------------------------------

func New(opts Options) *Pool {
	// Validate and sanitize options
	if opts.Concurrency < 1 {
		opts.Concurrency = runtime.GOMAXPROCS(0)
	}
	if opts.MaxCapacity < 0 {
		opts.MaxCapacity = opts.Concurrency
	}

	// Create pool
	p := Pool{
		rp:           rundownprotection.Create(),
		wg:           sync.WaitGroup{},
		shutdownSync: sync.Once{},

		maxCapacity:   opts.MaxCapacity,
		onCanceledJob: opts.OnCanceledJob,

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

func (p *Pool) Stop() {
	p.shutdownSync.Do(func() {
		// Initiate shutdown
		p.rp.Wait()
		p.wg.Wait()

		// Cancel jobs that were queued in a worker queue but not executed yet and close channels
		for _, ch := range p.workerJobQueue {
			// and
			if p.onCanceledJob != nil {
				for loop := true; loop; {
					select {
					case job := <-ch:
						p.onCanceledJob(job.id)
					default:
						loop = false
					}
				}
			}

			close(ch)
		}

		// Cancel pending jobs on the main queue
		for {
			job, ok := p.popJobNoLock() // No need to lock because no other routine accessing it
			if !ok {
				break // End the loop if no more jobs
			}
			if p.onCanceledJob != nil {
				p.onCanceledJob(job.id)
			}
		}

		// Misc cleanup
		close(p.idleQueue)

	})
}

func (p *Pool) QueueJob(id string, fn JobRoutine) bool {
	if !p.rp.Acquire() {
		return false
	}
	defer p.rp.Release()

	// Add to jobs list
	p.jobsListMtx.Lock()
	if p.maxCapacity > 0 && p.jobsList.Len() >= p.maxCapacity {
		p.jobsListMtx.Unlock()
		return false
	}
	p.jobsList.PushBack(Job{
		id: id,
		fn: fn,
	})
	p.jobsListMtx.Unlock()
	p.jobsAvailableEv.Set()

	// Done
	return true
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

		case job := <-p.workerJobQueue[workerID]:
			job.fn(p.rp, workerID+1, job.id)

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
