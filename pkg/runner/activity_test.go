package runner

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/internal/testutil"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type activityClient struct {
	*reposClient
	release <-chan struct{}
}

func (c activityClient) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	if name == "slow" {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.reposClient.FetchRepo(ctx, owner, name)
}

func TestActivityReportsCompletionBeforeOrderedOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	release := make(chan struct{})
	c := activityClient{&reposClient{repos: map[string]*fakeClient{
		"slow": {repo: testutil.Repo()}, "fast": {repo: testutil.Repo()},
	}}, release}
	r := newRunner(c, false)
	events := make(chan Activity, 4)
	r.Activity = func(a Activity) { events <- a }
	printed := make(chan string, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, []config.Repo{config.NewRepo("o", "slow", config.Builtin), config.NewRepo("o", "fast", config.Builtin)}, func(result RepoResult) { printed <- result.Repo })
	}()
	defer func() { cancel(); <-done }()
	for {
		select {
		case a := <-events:
			if a.Repo != "o/fast" || a.Phase != "repository-finished" {
				continue
			}
			if len(printed) != 0 {
				t.Fatal("ordered callback ran before first repository finished")
			}
			close(release)
			<-done
			if first, second := <-printed, <-printed; first != "o/slow" || second != "o/fast" {
				t.Fatal(first, second)
			}
			return
		case <-ctx.Done():
			t.Fatal("no live completion for fast repository")
		}
	}
}

type activityReviewer func(context.Context, review.Request) (policy.Verdict, error)

func (f activityReviewer) Review(ctx context.Context, req review.Request) (policy.Verdict, error) {
	return f(ctx, req)
}

func TestReviewActivityDistinguishesQueuedCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := make(chan Activity, 8)
	r := &Runner{ReviewParallel: 1, Activity: func(a Activity) { events <- a }, Reviewer: activityReviewer(func(ctx context.Context, _ review.Request) (policy.Verdict, error) {
		<-ctx.Done()
		return policy.Verdict{}, ctx.Err()
	})}
	done := make(chan error, 2)
	start := func(number int) {
		_, err := r.review(ctx, review.Request{Owner: "o", Name: "r", Number: number})
		done <- err
	}
	go start(1)
	waitEvent := func(number int, phase string) {
		t.Helper()
		select {
		case a := <-events:
			if a.PR != number || a.Phase != phase {
				t.Fatalf("unexpected event: %+v", a)
			}
		case <-ctx.Done():
			t.Fatal("missing activity event")
		}
	}
	waitEvent(1, "review-queued")
	waitEvent(1, "review-started")
	go start(2)
	waitEvent(2, "review-queued")
	cancel()
	for range 2 {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	for range 2 {
		if a := <-events; a.Phase != "review-finished" {
			t.Fatalf("queued review started after cancellation: %+v", a)
		}
	}
}

func TestReviewActivityFinishesOnSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var phases []string
		r := &Runner{Activity: func(a Activity) { phases = append(phases, a.Phase) }, Reviewer: &fakeReviewer{fail: map[int]bool{1: fail}}}
		_, _ = r.review(context.Background(), review.Request{Owner: "o", Name: "r", Number: 1})
		if !slices.Equal(phases, []string{"review-queued", "review-started", "review-finished"}) {
			t.Fatal(phases)
		}
	}
}
