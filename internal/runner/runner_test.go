package runner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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

func (f *fakeClient) FetchRepo(ctx context.Context, _, _ string) (*github.Repo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("Post \"https://api.github.com/graphql\": %w", err)
	}
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

func TestApplyRequestsOneRebaseAtATime(t *testing.T) {
	var prs []github.PullRequest
	for n := 1; n <= 3; n++ {
		pr := testutil.PR(n)
		pr.MergeState = "BEHIND"
		prs = append(prs, pr)
	}
	fc := &fakeClient{repo: testutil.Repo(prs...)}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.comments) != 1 || len(fc.comments[1]) != 1 {
		t.Fatalf("comments = %v; only #1 should be rebased", fc.comments)
	}
	if len(res.Events) != 1 || res.Events[0].Kind != EventRebaseRequested {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestApplyFallsBackToAllowedMergeMethod(t *testing.T) {
	repo := testutil.Repo(testutil.PR(1, "lib:minor"))
	repo.SquashAllowed, repo.MergeAllowed = false, true
	fc := &fakeClient{repo: repo}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if res.MergeMethod != "merge" {
		t.Fatalf("merge method = %q", res.MergeMethod)
	}
	if len(res.Events) != 1 || res.Events[0].Detail != "merge" || res.Events[0].Grade != config.UpdateMinor {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestInterruptedRunSaysSo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1))}
	res := newRunner(fc, true).RunRepo(ctx, repoCfg)
	if res.Error != "interrupted" {
		t.Fatalf("error = %q", res.Error)
	}
}

// reposClient serves one fakeClient per repo name and records whether two
// writes ever overlapped.
type reposClient struct {
	repos             map[string]*fakeClient
	writing, overlaps atomic.Int32
	delay             time.Duration
}

func (c *reposClient) Viewer(context.Context) (string, error) { return "me", nil }

func (c *reposClient) FetchRepo(ctx context.Context, _, name string) (*github.Repo, error) {
	time.Sleep(c.delay)
	return c.repos[name].FetchRepo(ctx, "", name)
}

func (c *reposClient) write(f func() error) error {
	if c.writing.Add(1) > 1 {
		c.overlaps.Add(1)
	}
	defer c.writing.Add(-1)
	time.Sleep(time.Millisecond)
	return f()
}

func (c *reposClient) Merge(ctx context.Context, _, name string, n int, method, sha string) error {
	return c.write(func() error { return c.repos[name].Merge(ctx, "", name, n, method, sha) })
}

func (c *reposClient) Comment(ctx context.Context, _, name string, n int, body string) error {
	return c.write(func() error { return c.repos[name].Comment(ctx, "", name, n, body) })
}

func TestRunKeepsOrderAndSerialisesWrites(t *testing.T) {
	c := &reposClient{repos: map[string]*fakeClient{}}
	var cfgs []config.Repo
	for i := range 6 {
		name := fmt.Sprintf("r%d", i)
		c.repos[name] = &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2))}
		cfgs = append(cfgs, config.Repo{Owner: "o", Name: name, Policy: config.Builtin})
	}
	var seen []string
	results := newRunner(c, true).Run(context.Background(), cfgs, func(r RepoResult) { seen = append(seen, r.Repo) })
	if len(results) != 6 || len(seen) != 6 {
		t.Fatalf("results = %d, progress = %v", len(results), seen)
	}
	for i, r := range results {
		if want := fmt.Sprintf("o/r%d", i); r.Repo != want || seen[i] != want {
			t.Fatalf("[%d] result %s, progress %s; want %s", i, r.Repo, seen[i], want)
		}
		if len(r.Events) != 2 {
			t.Fatalf("%s events = %+v", r.Repo, r.Events)
		}
	}
	if n := c.overlaps.Load(); n != 0 {
		t.Fatalf("%d writes overlapped another", n)
	}
}

func TestRunStopsStartingReposWhenInterrupted(t *testing.T) {
	c := &reposClient{repos: map[string]*fakeClient{}, delay: 5 * time.Millisecond}
	var cfgs []config.Repo
	for i := range 6 {
		name := fmt.Sprintf("r%d", i)
		c.repos[name] = &fakeClient{repo: testutil.Repo(testutil.PR(1))}
		cfgs = append(cfgs, config.Repo{Owner: "o", Name: name, Policy: config.Builtin})
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := newRunner(c, false)
	r.Parallel = 1
	results := r.Run(ctx, cfgs, func(RepoResult) { cancel() })
	if len(results) == 0 || len(results) >= 6 {
		t.Fatalf("got %d results; want the run to stop early", len(results))
	}
}

func TestEventsCarryPRURL(t *testing.T) {
	pr := testutil.PR(1)
	pr.URL = "https://github.com/o/r/pull/1"
	res := newRunner(&fakeClient{repo: testutil.Repo(pr)}, true).RunRepo(context.Background(), repoCfg)
	if len(res.Events) != 1 || res.Events[0].URL != pr.URL {
		t.Fatalf("events = %+v", res.Events)
	}
}
