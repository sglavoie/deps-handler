package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/policy"
	"github.com/sglavoie/deps-merger/internal/review"
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
	// mergeErrs fail a PR's next merges with these errors, in order;
	// mergedAnyway makes such a failure merge the PR all the same, as a
	// merge that timed out on its way back can.
	mergeErrs    map[int][]error
	mergedAnyway map[int]bool
	// fetchErrs are returned by the next fetches, in order.
	fetchErrs []error
}

func (f *fakeClient) Viewer(context.Context) (string, error) { return "me", nil }

func (f *fakeClient) FetchRepo(ctx context.Context, _, _ string) (*github.Repo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("Post \"https://api.github.com/graphql\": %w", err)
	}
	if len(f.fetchErrs) > 0 {
		err := f.fetchErrs[0]
		f.fetchErrs = f.fetchErrs[1:]
		return nil, err
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
	var err error
	if errs := f.mergeErrs[n]; len(errs) > 0 {
		err, f.mergeErrs[n] = errs[0], errs[1:]
		if !f.mergedAnyway[n] {
			return err
		}
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
	return err
}

func (f *fakeClient) Comment(_ context.Context, _, _ string, n int, body string) error {
	if f.comments == nil {
		f.comments = map[int][]string{}
	}
	f.comments[n] = append(f.comments[n], body)
	return nil
}

func (f *fakeClient) PullRequestDetails(context.Context, string, string, int) (*github.PullRequestDetails, error) {
	return &github.PullRequestDetails{Body: "release notes"}, nil
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

func TestFetchRetriesTransientFailureOnce(t *testing.T) {
	bad := &api.HTTPError{StatusCode: 502}
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), fetchErrs: []error{bad}}
	if res := newRunner(fc, true).RunRepo(context.Background(), repoCfg); res.Error != "" || len(fc.merges) != 1 {
		t.Fatalf("error %q, merges %v; one 502 should be retried", res.Error, fc.merges)
	}
	fc = &fakeClient{repo: testutil.Repo(testutil.PR(1)), fetchErrs: []error{bad, bad}}
	if res := newRunner(fc, true).RunRepo(context.Background(), repoCfg); res.Error == "" {
		t.Fatal("a second 502 should fail the repo")
	}
	fc = &fakeClient{repo: testutil.Repo(testutil.PR(1)), fetchErrs: []error{&api.HTTPError{StatusCode: 404}}}
	if res := newRunner(fc, true).RunRepo(context.Background(), repoCfg); res.Error == "" {
		t.Fatal("a 404 should not be retried")
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

func (c *reposClient) PullRequestDetails(ctx context.Context, _, name string, n int) (*github.PullRequestDetails, error) {
	return c.repos[name].PullRequestDetails(ctx, "", name, n)
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

// fakeReviewer answers with a verdict per PR number, approving by default.
type fakeReviewer struct {
	verdicts map[int]policy.Verdict
	fail     map[int]bool
	// errs fail a PR's next reviews with these errors, in order.
	errs  map[int][]error
	calls []int
	// onReview runs before each review, for checking what happened first.
	onReview func(review.Request)
}

func (f *fakeReviewer) Review(_ context.Context, req review.Request) (policy.Verdict, error) {
	f.calls = append(f.calls, req.Number)
	if f.onReview != nil {
		f.onReview(req)
	}
	if f.fail[req.Number] {
		return policy.Verdict{}, errors.New("claude: exit status 1")
	}
	if errs := f.errs[req.Number]; len(errs) > 0 {
		f.errs[req.Number] = errs[1:]
		return policy.Verdict{}, errs[0]
	}
	if v, ok := f.verdicts[req.Number]; ok {
		return v, nil
	}
	return policy.Verdict{Approved: true, Summary: "no breaking change used here"}, nil
}

func reviewRunner(c github.Client, rv review.Reviewer, apply bool) *Runner {
	r := newRunner(c, apply)
	r.Reviewer, r.AgentName = rv, "claude"
	return r
}

func TestAgentApprovedMajorMergesAfterSafePRs(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"), testutil.PR(2))}
	rv := &fakeReviewer{onReview: func(review.Request) {
		if len(c.merges) != 1 {
			t.Errorf("review ran before the safe PR merged: merges %v", c.merges)
		}
	}}
	res := reviewRunner(c, rv, true).RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(c.merges) != "[2 1]" || fmt.Sprint(rv.calls) != "[1]" || res.Failed() {
		t.Fatalf("merges %v, reviews %v, result %+v", c.merges, rv.calls, res)
	}
}

func TestAgentRejectedMajorIsLeft(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rv := &fakeReviewer{verdicts: map[int]policy.Verdict{1: {Summary: "removes v1 API", Concerns: []string{"src/a.ts uses it"}}}}
	res := reviewRunner(c, rv, true).RunRepo(context.Background(), repoCfg)
	if len(c.merges) != 0 || res.Decisions[0].Action != policy.ActionSkip || res.Failed() {
		t.Fatalf("merges %v, decisions %+v", c.merges, res.Decisions)
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != policy.FindingAgentRejected {
		t.Fatalf("findings %+v", res.Findings)
	}
}

func TestAgentReviewsInDryRun(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rv := &fakeReviewer{}
	res := reviewRunner(c, rv, false).RunRepo(context.Background(), repoCfg)
	d := res.Decisions[0]
	if len(c.merges) != 0 || d.Action != policy.ActionMerge || d.Verdict == nil || !d.Verdict.Approved {
		t.Fatalf("merges %v, decision %+v", c.merges, d)
	}
}

func TestAgentFailureFailsTheRun(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rv := &fakeReviewer{fail: map[int]bool{1: true}}
	res := reviewRunner(c, rv, true).RunRepo(context.Background(), repoCfg)
	if len(c.merges) != 0 || len(rv.calls) != 1 || !res.Failed() || res.Decisions[0].Action != policy.ActionSkip {
		t.Fatalf("merges %v, reviews %v, decisions %+v", c.merges, rv.calls, res.Decisions)
	}
}

func TestAgentReviewLimit(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"), testutil.PR(2, "lib:major"))}
	rv := &fakeReviewer{}
	r := reviewRunner(c, rv, true)
	r.MaxReviews = 1
	res := r.RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(c.merges) != "[1]" || fmt.Sprint(rv.calls) != "[1]" {
		t.Fatalf("merges %v, reviews %v", c.merges, rv.calls)
	}
	d := res.Decisions[0]
	if d.Number != 2 || d.Action != policy.ActionReview || !strings.Contains(d.Reason, "review limit of 1 reached") {
		t.Fatalf("decision %+v", d)
	}
}

func TestAgentVerdictsAreCached(t *testing.T) {
	cache, err := review.OpenCache(t.TempDir() + "/verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	pr := testutil.PR(1, "lib:major")
	rv := &fakeReviewer{verdicts: map[int]policy.Verdict{1: {Summary: "no"}}}
	for range 2 {
		r := reviewRunner(&fakeClient{repo: testutil.Repo(pr)}, rv, false)
		r.Verdicts = cache
		if res := r.RunRepo(context.Background(), repoCfg); res.Decisions[0].Action != policy.ActionSkip {
			t.Fatalf("decision %+v", res.Decisions[0])
		}
	}
	if len(rv.calls) != 1 {
		t.Fatalf("reviews %v; want the second run to reuse the verdict", rv.calls)
	}
}

func TestTimedOutMergeThatWentThroughIsReportedMerged(t *testing.T) {
	fc := &fakeClient{
		repo:         testutil.Repo(testutil.PR(1)),
		mergeErrs:    map[int][]error{1: {&api.HTTPError{StatusCode: 502}}},
		mergedAnyway: map[int]bool{1: true},
	}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(fc.merges) != "[1]" || len(res.Events) != 1 || res.Events[0].Kind != EventMerged || res.Failed() {
		t.Fatalf("merges %v, events %+v", fc.merges, res.Events)
	}
	if !strings.Contains(res.Events[0].Detail, "squash, after") {
		t.Fatalf("detail %q should say GitHub failed first", res.Events[0].Detail)
	}
}

func TestTimedOutMergeThatDidNotGoThroughIsRetried(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {&api.HTTPError{StatusCode: 504}}}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(fc.merges) != "[1]" || len(res.Events) != 1 || res.Events[0].Detail != "squash" || res.Failed() {
		t.Fatalf("merges %v, events %+v", fc.merges, res.Events)
	}
}

func TestRateLimitedWriteIsMadeAgain(t *testing.T) {
	limited := &api.HTTPError{StatusCode: 403, Message: "secondary rate limit", Headers: map[string][]string{"Retry-After": {"20"}}}
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {limited}}}
	r := newRunner(fc, true)
	var slept []time.Duration
	r.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	res := r.RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(fc.merges) != "[1]" || res.Failed() || len(slept) == 0 || slept[0] != 20*time.Second {
		t.Fatalf("merges %v, slept %v, events %+v", fc.merges, slept, res.Events)
	}
}

func TestAgentApprovalIsNamedOnTheMerge(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	res := reviewRunner(c, &fakeReviewer{}, true).RunRepo(context.Background(), repoCfg)
	if len(res.Events) != 1 || res.Events[0].Detail != "squash, approved by claude" {
		t.Fatalf("events %+v", res.Events)
	}
}

func TestHeadMovedDuringReviewIsReviewedAgain(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rv := &fakeReviewer{errs: map[int][]error{1: {fmt.Errorf("checkout: %w", review.ErrHeadMoved)}}}
	r := reviewRunner(c, rv, true)
	r.MaxReviews = 1
	res := r.RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(c.merges) != "[1]" || fmt.Sprint(rv.calls) != "[1 1]" || res.Failed() {
		t.Fatalf("merges %v, reviews %v, result %+v", c.merges, rv.calls, res)
	}
}

func TestHeadThatKeepsMovingWaitsForNextRun(t *testing.T) {
	moved := fmt.Errorf("checkout: %w", review.ErrHeadMoved)
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rv := &fakeReviewer{errs: map[int][]error{1: {moved, moved, moved}}}
	res := reviewRunner(c, rv, true).RunRepo(context.Background(), repoCfg)
	d := res.Decisions[0]
	if len(c.merges) != 0 || len(rv.calls) != 2 || res.Failed() || d.Action != policy.ActionReview || !strings.Contains(d.Reason, "head moved") {
		t.Fatalf("merges %v, reviews %v, decision %+v", c.merges, rv.calls, d)
	}
}

func TestUncachedVerdictIsAWarning(t *testing.T) {
	dir := t.TempDir()
	cache, err := review.OpenCache(dir + "/state/verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	// A file where the state directory should be makes every write fail.
	if err := os.WriteFile(dir+"/state", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	r := reviewRunner(c, &fakeReviewer{}, true)
	r.Verdicts = cache
	res := r.RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(c.merges) != "[1]" || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "#1: verdict not cached") {
		t.Fatalf("merges %v, warnings %q", c.merges, res.Warnings)
	}
}

func TestCachedRejectionLapsesWhenTheHeadMoves(t *testing.T) {
	cache, err := review.OpenCache(t.TempDir() + "/verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	pr := testutil.PR(1, "lib:major")
	rv := &fakeReviewer{verdicts: map[int]policy.Verdict{1: {Summary: "no"}}}
	for _, head := range []string{"sha1", "sha1", "rebased"} {
		pr.HeadOID, pr.ChecksOID = head, head
		r := reviewRunner(&fakeClient{repo: testutil.Repo(pr)}, rv, false)
		r.Verdicts = cache
		r.RunRepo(context.Background(), repoCfg)
	}
	if len(rv.calls) != 2 {
		t.Fatalf("reviews %v; want the rebase alone to bring a new review", rv.calls)
	}
}
