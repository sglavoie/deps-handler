package jobs_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/internal/jobs"
	"github.com/sglavoie/deps-handler/internal/store"
)

func journal(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func receive(t *testing.T, ch <-chan jobs.Job) jobs.Job {
	t.Helper()
	select {
	case j := <-ch:
		return j
	case <-time.After(2 * time.Second):
		t.Fatal("queued job did not run")
		return jobs.Job{}
	}
}

func TestPendingJobSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &jobs.Queues{Journal: s, Debounce: time.Hour,
		Run: func(context.Context, jobs.Job) error { t.Error("first process ran the job before restart"); return nil }}
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want := jobs.Job{Installation: 1, Repo: "o/r", Trigger: "webhook"}
	if err := q.Enqueue(want); err != nil {
		t.Fatal(err)
	}
	if saved, err := s.PendingJobs(ctx); err != nil || len(saved) != 1 {
		t.Fatalf("saved before restart = %+v, %v", saved, err)
	}
	cancel()
	q.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ran := make(chan jobs.Job, 1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	q2 := &jobs.Queues{Journal: s, Now: func() time.Time { return time.Now().Add(2 * time.Hour) },
		Run: func(_ context.Context, j jobs.Job) error { ran <- j; return nil }}
	if err := q2.Start(ctx2); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel2(); q2.Wait() }()
	if got := receive(t, ran); got != want {
		t.Fatalf("replayed job = %+v, want %+v", got, want)
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		saved, err := s.PendingJobs(ctx2)
		if err != nil {
			t.Fatal(err)
		}
		if len(saved) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed job still saved: %+v", saved)
		}
	}
	cancel2()
}

func TestInterruptedRunReplaysButNewEventIsSeparate(t *testing.T) {
	s := journal(t)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	q := &jobs.Queues{Journal: s, Run: func(ctx context.Context, _ jobs.Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(jobs.Job{Installation: 2, Repo: "o/r", Trigger: "first"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first run did not start")
	}
	// GitHub webhooks and repository listings can spell the same name
	// differently. Replay must still coalesce both accepted events.
	if err := q.Enqueue(jobs.Job{Installation: 2, Repo: "O/R", Trigger: "second"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	q.Wait()
	saved, err := s.PendingJobs(context.Background())
	if err != nil || len(saved) != 2 {
		t.Fatalf("interrupted events = %+v, %v", saved, err)
	}

	var calls atomic.Int32
	ctx2, cancel2 := context.WithCancel(context.Background())
	q2 := &jobs.Queues{Journal: s, Run: func(context.Context, jobs.Job) error { calls.Add(1); return nil }}
	if err := q2.Start(ctx2); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		saved, err = s.PendingJobs(ctx2)
		if err != nil {
			t.Fatal(err)
		}
		if len(saved) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replayed events still saved: %+v", saved)
		}
	}
	cancel2()
	q2.Wait()
	if calls.Load() != 1 {
		t.Fatalf("replayed runs = %d, want one coalesced run", calls.Load())
	}
}

func TestFinishingRunKeepsEventReceivedDuringIt(t *testing.T) {
	s := journal(t)
	firstStarted, firstDone := make(chan struct{}), make(chan struct{})
	secondStarted, secondDone := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	q := &jobs.Queues{Journal: s, Run: func(context.Context, jobs.Job) error {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-firstDone
		} else {
			close(secondStarted)
			<-secondDone
		}
		return nil
	}}
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-firstDone:
		default:
			close(firstDone)
		}
		select {
		case <-secondDone:
		default:
			close(secondDone)
		}
		cancel()
		q.Wait()
	}()
	if err := q.Enqueue(jobs.Job{Installation: 4, Repo: "o/r", Trigger: "first"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first run did not start")
	}
	if err := q.Enqueue(jobs.Job{Installation: 4, Repo: "o/r", Trigger: "second"}); err != nil {
		t.Fatal(err)
	}
	close(firstDone)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second run did not start")
	}
	saved, err := s.PendingJobs(ctx)
	if err != nil || len(saved) != 1 || saved[0].Job.Trigger != "second" {
		t.Fatalf("event during first run = %+v, %v", saved, err)
	}
	close(secondDone)
}

func TestFailedRunUsesBoundedRetriesAndAcknowledgesAfterExhaustion(t *testing.T) {
	s := journal(t)
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	q := &jobs.Queues{Journal: s, RetryDelays: []time.Duration{time.Millisecond},
		Run: func(context.Context, jobs.Job) error { calls.Add(1); return errors.New("failed") }}
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(jobs.Job{Installation: 3, Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		saved, err := s.PendingJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(saved) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exhausted job still saved: %+v", saved)
		}
	}
	cancel()
	q.Wait()
	if calls.Load() != 2 {
		t.Fatalf("runs = %d, want initial and one retry", calls.Load())
	}
}
