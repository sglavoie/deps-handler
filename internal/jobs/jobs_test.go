package jobs

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recorder struct {
	mu   sync.Mutex
	jobs []Job
	// active counts runs in progress per installation; overlap is set if
	// two of one installation ever ran at once.
	active  map[int64]*atomic.Int32
	overlap atomic.Bool
	hold    time.Duration
}

func (r *recorder) run(_ context.Context, j Job) {
	r.mu.Lock()
	r.jobs = append(r.jobs, j)
	a, ok := r.active[j.Installation]
	if !ok {
		a = &atomic.Int32{}
		r.active[j.Installation] = a
	}
	r.mu.Unlock()
	if a.Add(1) > 1 {
		r.overlap.Store(true)
	}
	time.Sleep(r.hold)
	a.Add(-1)
}

func (r *recorder) seen() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.jobs)
}

func start(t *testing.T, debounce, hold time.Duration) (*Queues, *recorder) {
	t.Helper()
	r := &recorder{active: map[int64]*atomic.Int32{}, hold: hold}
	q := &Queues{Run: r.run, Debounce: debounce}
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	t.Cleanup(func() { cancel(); q.Wait() })
	return q, r
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBurstBecomesOneRun(t *testing.T) {
	q, r := start(t, 50*time.Millisecond, 0)
	for range 5 {
		q.Enqueue(Job{Installation: 1, Repo: "o/a", Trigger: "check_suite"})
	}
	q.Enqueue(Job{Installation: 1, Repo: "o/b", Trigger: "push"})
	eventually(t, "both repos to run", func() bool { return len(r.seen()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if got := r.seen(); len(got) != 2 {
		t.Fatalf("runs = %+v", got)
	}
	// An event after the run started queues another.
	q.Enqueue(Job{Installation: 1, Repo: "o/a", Trigger: "push"})
	eventually(t, "the third run", func() bool { return len(r.seen()) == 3 })
}

func TestInstallationRunsNeverOverlap(t *testing.T) {
	q, r := start(t, 0, 20*time.Millisecond)
	for _, repo := range []string{"o/a", "o/b", "o/c"} {
		q.Enqueue(Job{Installation: 1, Repo: repo})
		q.Enqueue(Job{Installation: 2, Repo: repo})
	}
	eventually(t, "six runs", func() bool { return len(r.seen()) == 6 })
	if r.overlap.Load() {
		t.Fatal("two runs of one installation overlapped")
	}
}

func TestInstallationsRunSideBySide(t *testing.T) {
	q, r := start(t, 0, 200*time.Millisecond)
	began := time.Now()
	q.Enqueue(Job{Installation: 1, Repo: "o/a"})
	q.Enqueue(Job{Installation: 2, Repo: "p/a"})
	eventually(t, "both runs", func() bool { return len(r.seen()) == 2 })
	// The second began while the first held its worker.
	if time.Since(began) > 150*time.Millisecond {
		t.Fatal("installations ran one after the other")
	}
}

func TestSweepAbsorbsSingleRepos(t *testing.T) {
	q, r := start(t, 50*time.Millisecond, 0)
	q.Enqueue(Job{Installation: 1, Repo: "o/a"})
	q.Enqueue(Job{Installation: 1, Repo: "", Trigger: "schedule"})
	q.Enqueue(Job{Installation: 1, Repo: "o/b"})
	eventually(t, "the sweep", func() bool { return len(r.seen()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := r.seen(); len(got) != 1 || got[0].Repo != "" || got[0].Trigger != "schedule" {
		t.Fatalf("runs = %+v", got)
	}
}

func TestNothingRunsAfterStop(t *testing.T) {
	r := &recorder{active: map[int64]*atomic.Int32{}}
	q := &Queues{Run: r.run, Debounce: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	q.Enqueue(Job{Installation: 1, Repo: "o/a"})
	cancel()
	q.Wait()
	q.Enqueue(Job{Installation: 1, Repo: "o/b"})
	if len(r.seen()) != 0 || q.Pending() != 1 {
		t.Fatalf("runs %v, pending %d", r.seen(), q.Pending())
	}
}
