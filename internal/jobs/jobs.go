// Package jobs queues the runs webhooks and the schedule ask for, one
// worker per installation, so that an installation's runs, and the writes
// they make, never overlap.
package jobs

import (
	"context"
	"sync"
	"time"
)

// Job is a run of one repository of an installation, or of all of them
// when Repo is empty.
type Job struct {
	Installation int64
	Repo         string
	Trigger      string
}

// Queues runs jobs through Run, one at a time per installation.
type Queues struct {
	// Run does a job; jobs of one installation never run at once.
	Run func(context.Context, Job)
	// Debounce is how long a job waits before it runs, so that a burst of
	// events about one repository, such as several check suites
	// finishing, becomes one run. Later events in that time join it.
	Debounce time.Duration
	// Now and After are time.Now and time.After, replaced in tests.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	ctx     context.Context
	mu      sync.Mutex
	workers map[int64]*worker
	wg      sync.WaitGroup
}

type worker struct {
	pending map[string]pendingJob
	wake    chan struct{}
}

type pendingJob struct {
	job Job
	due time.Time
}

// Start lets the queues run jobs until ctx ends; Wait then waits for the
// workers to stop.
func (q *Queues) Start(ctx context.Context) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ctx = ctx
	if q.Now == nil {
		q.Now = time.Now
	}
	if q.After == nil {
		q.After = time.After
	}
	q.workers = map[int64]*worker{}
}

// Wait waits for every worker to stop, once the context Start was given
// has ended.
func (q *Queues) Wait() { q.wg.Wait() }

// Enqueue asks for job to run once Debounce has passed. A job for the same
// repository already waiting absorbs it; a job for all of an
// installation's repositories absorbs those of single ones.
func (q *Queues) Enqueue(job Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx == nil || q.ctx.Err() != nil {
		return
	}
	w, ok := q.workers[job.Installation]
	if !ok {
		w = &worker{pending: map[string]pendingJob{}, wake: make(chan struct{}, 1)}
		q.workers[job.Installation] = w
		q.wg.Add(1)
		go q.work(job.Installation, w)
	}
	if _, all := w.pending[""]; all {
		return
	}
	if _, waiting := w.pending[job.Repo]; !waiting {
		w.pending[job.Repo] = pendingJob{job: job, due: q.Now().Add(q.Debounce)}
	}
	if job.Repo == "" {
		for repo := range w.pending {
			if repo != "" {
				delete(w.pending, repo)
			}
		}
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Pending counts the jobs waiting to run.
func (q *Queues) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, w := range q.workers {
		n += len(w.pending)
	}
	return n
}

func (q *Queues) work(id int64, w *worker) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		var next *pendingJob
		for _, p := range w.pending {
			if next == nil || p.due.Before(next.due) {
				next = &p
			}
		}
		var wait time.Duration
		if next != nil {
			wait = next.due.Sub(q.Now())
			if wait <= 0 {
				delete(w.pending, next.job.Repo)
			}
		}
		q.mu.Unlock()

		switch {
		case next != nil && wait <= 0:
			q.Run(q.ctx, next.job)
		case next != nil:
			select {
			case <-q.ctx.Done():
				return
			case <-w.wake:
			case <-q.After(wait):
			}
		default:
			select {
			case <-q.ctx.Done():
				return
			case <-w.wake:
			}
		}
		if q.ctx.Err() != nil {
			return
		}
	}
}
