// Package jobs queues the runs webhooks and the schedule ask for, one
// worker per installation, so that an installation's runs, and the writes
// they make, never overlap.
package jobs

import (
	"context"
	"errors"
	"fmt"
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

// SavedJob is one accepted event that has not yet been processed. A run
// may cover several events, but each keeps its own ID until that run ends.
type SavedJob struct {
	ID  int64
	Job Job
	Due time.Time
}

// Journal keeps accepted events across process restarts. Append must commit
// before it returns: the webhook acknowledges a delivery only afterwards.
type Journal interface {
	AppendJob(context.Context, Job, time.Time) (int64, error)
	PendingJobs(context.Context) ([]SavedJob, error)
	DeleteJobs(context.Context, []int64) error
}

// Queues runs jobs through Run, one at a time per installation.
type Queues struct {
	// Run does a job; jobs of one installation never run at once.
	Run func(context.Context, Job) error
	// Debounce is how long a job waits before it runs, so that a burst of
	// events about one repository, such as several check suites
	// finishing, becomes one run. Later events in that time join it.
	Debounce time.Duration
	// RetryDelays bounds retries after a failed run. Nil uses the standard
	// delays; an empty non-nil slice disables retries.
	RetryDelays []time.Duration
	// Now and After are time.Now and time.After, replaced in tests.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Journal, when set, replays accepted events on startup and acknowledges
	// them only after their run finishes. OnError reports journal failures
	// that happen after a webhook has already been acknowledged.
	Journal Journal
	OnError func(error)

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
	job      Job
	due      time.Time
	attempts int
	ids      []int64
}

var defaultRetryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Start lets the queues run jobs until ctx ends; Wait then waits for the
// workers to stop.
func (q *Queues) Start(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	var saved []SavedJob
	if q.Journal != nil {
		var err error
		saved, err = q.Journal.PendingJobs(ctx)
		if err != nil {
			return fmt.Errorf("load queued jobs: %w", err)
		}
	}
	q.ctx = ctx
	if q.Now == nil {
		q.Now = time.Now
	}
	if q.After == nil {
		q.After = time.After
	}
	if q.RetryDelays == nil {
		q.RetryDelays = defaultRetryDelays
	}
	q.workers = map[int64]*worker{}
	for _, entry := range saved {
		q.enqueueLocked(entry.Job, entry.Due, entry.ID)
	}
	return nil
}

// Wait waits for every worker to stop, once the context Start was given
// has ended.
func (q *Queues) Wait() { q.wg.Wait() }

// Enqueue asks for job to run once Debounce has passed. A job for the same
// repository already waiting absorbs it; a job for all of an
// installation's repositories absorbs those of single ones.
func (q *Queues) Enqueue(job Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx == nil {
		return errors.New("job queue has not started")
	}
	if err := q.ctx.Err(); err != nil {
		return err
	}
	due := q.Now().Add(q.Debounce)
	var id int64
	if q.Journal != nil {
		var err error
		id, err = q.Journal.AppendJob(q.ctx, job, due)
		if err != nil {
			return fmt.Errorf("save queued job: %w", err)
		}
	}
	q.enqueueLocked(job, due, id)
	return nil
}

func (q *Queues) enqueueLocked(job Job, due time.Time, id int64) {
	w, ok := q.workers[job.Installation]
	if !ok {
		w = &worker{pending: map[string]pendingJob{}, wake: make(chan struct{}, 1)}
		q.workers[job.Installation] = w
		q.wg.Add(1)
		go q.work(job.Installation, w)
	}
	if p, all := w.pending[""]; all {
		if id != 0 {
			p.ids = append(p.ids, id)
		}
		if p.attempts > 0 {
			// A fresh event should not wait for an older retry timer.
			p.due = minTime(p.due, due)
			p.attempts = 0
		}
		w.pending[""] = p
		q.wake(w)
		return
	}
	if p, waiting := w.pending[job.Repo]; waiting && p.attempts > 0 {
		p.due = minTime(p.due, due)
		p.attempts = 0
		p.job = job
		if id != 0 {
			p.ids = append(p.ids, id)
		}
		w.pending[job.Repo] = p
	} else if waiting {
		if id != 0 {
			p.ids = append(p.ids, id)
			w.pending[job.Repo] = p
		}
	} else if !waiting {
		p := pendingJob{job: job, due: due}
		if id != 0 {
			p.ids = []int64{id}
		}
		w.pending[job.Repo] = p
	}
	if job.Repo == "" {
		all := w.pending[""]
		for repo, pending := range w.pending {
			if repo != "" {
				all.ids = append(all.ids, pending.ids...)
				delete(w.pending, repo)
			}
		}
		w.pending[""] = all
	}
	q.wake(w)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (q *Queues) wake(w *worker) {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (q *Queues) retry(w *worker, p pendingJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx.Err() != nil || p.attempts >= len(q.RetryDelays) {
		return
	}
	if _, all := w.pending[""]; all {
		current := w.pending[""]
		current.ids = append(current.ids, p.ids...)
		w.pending[""] = current
		return
	}
	if current, waiting := w.pending[p.job.Repo]; waiting {
		current.ids = append(current.ids, p.ids...)
		w.pending[p.job.Repo] = current
		return
	}
	p.due = q.Now().Add(q.RetryDelays[p.attempts])
	p.attempts++
	if p.job.Repo == "" {
		for repo, pending := range w.pending {
			p.due = minTime(p.due, pending.due)
			p.ids = append(p.ids, pending.ids...)
			delete(w.pending, repo)
		}
	}
	w.pending[p.job.Repo] = p
	q.wake(w)
}

func (q *Queues) acknowledge(p pendingJob) error {
	if q.Journal == nil || len(p.ids) == 0 || q.ctx.Err() != nil {
		return nil
	}
	if err := q.Journal.DeleteJobs(q.ctx, p.ids); err != nil {
		return fmt.Errorf("acknowledge queued job: %w", err)
	}
	return nil
}

func (q *Queues) report(err error) {
	if q.OnError != nil {
		q.OnError(err)
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
		// Retire an idle worker while holding the same lock Enqueue uses.
		// An event racing with retirement either joins this worker before
		// it checks pending, or creates a new one after it is removed.
		if len(w.pending) == 0 {
			delete(q.workers, id)
			q.mu.Unlock()
			return
		}
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
		if q.ctx.Err() != nil {
			return
		}

		switch {
		case next != nil && wait <= 0:
			err := q.Run(q.ctx, next.job)
			if err == nil {
				err = q.acknowledge(*next)
				if err != nil {
					q.report(err)
				}
			}
			if err != nil {
				if next.attempts >= len(q.RetryDelays) {
					if aerr := q.acknowledge(*next); aerr != nil {
						q.report(aerr)
					}
				}
				q.retry(w, *next)
			}
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
