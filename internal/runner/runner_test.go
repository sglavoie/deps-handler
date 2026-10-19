package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/testutil"
)

// fakeClient serves a mutable repo; merging removes the PR and, like
// GitHub, briefly leaves the others' mergeability UNKNOWN.
type fakeClient struct {
	repo      *github.Repo
	merges    []int
	comments  map[int][]string
	failMerge map[int]bool
	// transient fails a PR's merge with this error that many times first.
	transient  map[int]int
	unknownFor int
}

func (f *fakeClient) Viewer(context.Context) (string, error) { return "me", nil }

func (f *fakeClient) FetchRepo(context.Context, string, string) (*github.Repo, error) {
	cp := *f.repo
	cp.PullRequests = append([]github.PullRequest(nil), f.repo.PullRequests...)
	if f.unknownFor > 0 {
		f.unknownFor--
		for i := range cp.PullRequests {
			cp.PullRequests[i].Mergeable = "UNKNOWN"
		}
	}
	return &cp, nil
}

func (f *fakeClient) Merge(_ context.Context, _, _ string, n int, _, _ string) error {
	if f.failMerge[n] {
		return errors.New("405 not mergeable")
	}
	if f.transient[n] > 0 {
		f.transient[n]--
		return &api.HTTPError{StatusCode: 405, Message: "Base branch was modified"}
	}
	f.merges = append(f.merges, n)
	var keep []github.PullRequest
	for _, pr := range f.repo.PullRequests {
		if pr.Number != n {
			keep = append(keep, pr)
		}
	}
	f.repo.PullRequests = keep
	f.unknownFor = 2
	return nil
}

func (f *fakeClient) Comment(_ context.Context, _, _ string, n int, body string) error {
	if f.comments == nil {
		f.comments = map[int][]string{}
	}
	f.comments[n] = append(f.comments[n], body)
	return nil
}

func newRunner(c github.Client, apply bool) *Runner {
	r := New(c, "me", apply)
	r.Sleep = func(context.Context, time.Duration) error { return nil }
	return r
}

var repoCfg = config.Repo{Owner: "o", Name: "r", Policy: config.Builtin}

func TestDryRunChangesNothing(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2))}
	res := newRunner(fc, false).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 0 || len(fc.comments) != 0 || len(res.Events) != 0 {
		t.Fatalf("dry run wrote: merges=%v comments=%v", fc.merges, fc.comments)
	}
	if len(res.Decisions) != 2 {
		t.Fatalf("decisions = %d", len(res.Decisions))
	}
}

func TestApplyMergesSequentiallyAndWaitsForRecompute(t *testing.T) {
	major := testutil.PR(3, "lib:major")
	behind := testutil.PR(4)
	behind.MergeState = "BEHIND"
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2), major, behind)}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)

	if len(fc.merges) != 2 || fc.merges[0] != 1 || fc.merges[1] != 2 {
		t.Fatalf("merges = %v", fc.merges)
	}
	if got := fc.comments[4]; len(got) != 1 || got[0] != "@dependabot rebase" {
		t.Fatalf("rebase comments = %v", fc.comments)
	}
	if len(fc.comments[3]) != 0 {
		t.Fatal("held major PR must be left untouched")
	}
	if res.Error != "" {
		t.Fatal(res.Error)
	}
}

func TestApplyContinuesAfterMergeFailure(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2)), failMerge: map[int]bool{1: true}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 1 || fc.merges[0] != 2 {
		t.Fatalf("merges = %v", fc.merges)
	}
	if res.Events[0].Kind != EventMergeFailed || res.Events[0].Number != 1 {
		t.Fatalf("events = %+v", res.Events)
	}
	if !res.Failed() {
		t.Fatal("a failed merge must mark the result failed")
	}
}

func TestApplyRetriesTransientMergeFailureOnce(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), transient: map[int]int{1: 1}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 1 || len(res.Events) != 1 || res.Events[0].Kind != EventMerged {
		t.Fatalf("merges = %v events = %+v", fc.merges, res.Events)
	}
	if res.Failed() {
		t.Fatal("a merge that succeeded on retry is not a failure")
	}
}

func TestApplyGivesUpAfterSecondTransientFailure(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), transient: map[int]int{1: 2}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 0 || len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed {
		t.Fatalf("merges = %v events = %+v", fc.merges, res.Events)
	}
}

func TestResultSlicesAreNeverNil(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo()}
	res := newRunner(fc, false).RunRepo(context.Background(), repoCfg)
	if res.Decisions == nil || res.Findings == nil || res.Events == nil {
		t.Fatalf("nil slice in %+v; JSON would print null", res)
	}
}

func TestDryRunPollsUnknownMergeability(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), unknownFor: 1}
	res := newRunner(fc, false).RunRepo(context.Background(), repoCfg)
	if len(res.Decisions) != 1 || res.Decisions[0].Action != "merge" {
		t.Fatalf("decisions = %+v", res.Decisions)
	}
}
