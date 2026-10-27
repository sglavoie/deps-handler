package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/sglavoie/deps-merger/internal/testutil"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/dependabot"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/review"
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
	// fetchErrs are returned by the next fetches, in order; Merge adds
	// fetchErrsAfterMerge to them once it merges.
	fetchErrs           []error
	fetchErrsAfterMerge []error
	// lag keeps a merged PR open in that many more fetches, as GitHub can
	// right after a merge; merging it again meanwhile fails with a 405.
	lag     int
	lagging map[int]int
	// commentErrs fail a PR's rebase requests.
	commentErrs map[int]error
	// closedAnyway makes a failure from mergeErrs close the PR without
	// merging it, as someone can meanwhile.
	closedAnyway map[int]bool
	// files are the files FileText finds, by "owner/name:path"; fileErr
	// fails it instead.
	files   map[string]string
	fileErr error
	// unknownAfterMerge leaves every PR's mergeability UNKNOWN for that
	// many fetches after any merge attempt, failed ones included.
	unknownAfterMerge int
	// stateErrs are returned by the next PR state reads, in order.
	stateErrs []error
	// fetches counts the reads of the repository.
	fetches int
	// conflictOnMerge puts the open PRs in conflict with the base each
	// merge moves, as a sibling bumping the same lockfile does.
	conflictOnMerge bool
}

func (f *fakeClient) Viewer(context.Context) (string, error) { return "me", nil }

func (f *fakeClient) FetchRepo(ctx context.Context, _, _ string) (*github.Repo, error) {
	f.fetches++
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("Post \"https://api.github.com/graphql\": %w", err)
	}
	if len(f.fetchErrs) > 0 {
		err := f.fetchErrs[0]
		f.fetchErrs = f.fetchErrs[1:]
		return nil, err
	}
	for n, left := range f.lagging {
		if left == 0 {
			f.drop(n)
			delete(f.lagging, n)
			continue
		}
		f.lagging[n]--
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
	if f.unknownAfterMerge > 0 {
		defer func() { f.unknownFor = f.unknownAfterMerge }()
	}
	if f.failMerge[n] {
		return errors.New("405 not mergeable")
	}
	if f.transient[n] > 0 {
		f.transient[n]--
		return &api.HTTPError{StatusCode: 405, Message: "Base branch was modified"}
	}
	if _, ok := f.lagging[n]; ok {
		return &api.HTTPError{StatusCode: 405, Message: "Pull Request is not mergeable"}
	}
	var err error
	if errs := f.mergeErrs[n]; len(errs) > 0 {
		err, f.mergeErrs[n] = errs[0], errs[1:]
		if f.closedAnyway[n] {
			f.drop(n)
		}
		if !f.mergedAnyway[n] {
			return err
		}
	}
	f.merges = append(f.merges, n)
	f.fetchErrs = append(f.fetchErrs, f.fetchErrsAfterMerge...)
	if f.lag > 0 {
		if f.lagging == nil {
			f.lagging = map[int]int{}
		}
		f.lagging[n] = f.lag
		return err
	}
	f.drop(n)
	f.unknownFor = 2
	if f.conflictOnMerge {
		f.repo.FetchedAt = f.repo.FetchedAt.Add(time.Second)
		for i := range f.repo.PullRequests {
			f.repo.PullRequests[i].Mergeable = "CONFLICTING"
			f.repo.PullRequests[i].MergeState = "DIRTY"
			f.repo.PullRequests[i].BaseMovedAt = f.repo.FetchedAt
		}
	}
	return err
}

func (f *fakeClient) drop(n int) {
	var keep []github.PullRequest
	for _, pr := range f.repo.PullRequests {
		if pr.Number != n {
			keep = append(keep, pr)
		}
	}
	f.repo.PullRequests = keep
}

func (f *fakeClient) Comment(_ context.Context, _, _ string, n int, body string) error {
	if err := f.commentErrs[n]; err != nil {
		return err
	}
	if f.comments == nil {
		f.comments = map[int][]string{}
	}
	f.comments[n] = append(f.comments[n], body)
	return nil
}

func (f *fakeClient) FileText(_ context.Context, owner, name, path string) ([]byte, error) {
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	if s, ok := f.files[owner+"/"+name+":"+path]; ok {
		return []byte(s), nil
	}
	return nil, nil
}

func (f *fakeClient) PullRequestDetails(context.Context, string, string, int) (*github.PullRequestDetails, error) {
	return &github.PullRequestDetails{Body: "release notes"}, nil
}

func (f *fakeClient) PullRequestState(_ context.Context, _, _ string, n int) (github.PRState, error) {
	if len(f.stateErrs) > 0 {
		err := f.stateErrs[0]
		f.stateErrs = f.stateErrs[1:]
		return "", err
	}
	switch {
	case slices.Contains(f.merges, n):
		return github.PRMerged, nil
	case slices.ContainsFunc(f.repo.PullRequests, func(pr github.PullRequest) bool { return pr.Number == n }):
		return github.PROpen, nil
	}
	return github.PRClosed, nil
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

func TestConflictFromOurOwnMergeWaitsForDependabot(t *testing.T) {
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2)), conflictOnMerge: true}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 1 || fc.merges[0] != 1 {
		t.Fatalf("merges = %v", fc.merges)
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Action != policy.ActionWait || !strings.Contains(res.Decisions[0].Reason, "waiting for Dependabot to rebase") {
		t.Fatalf("decisions = %+v", res.Decisions)
	}
	if len(res.Findings) != 0 || res.Failed() {
		t.Fatalf("a conflict Dependabot is about to fix needs no attention: findings %+v", res.Findings)
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
	fileReads         atomic.Int32
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

func (c *reposClient) FileText(context.Context, string, string, string) ([]byte, error) {
	c.fileReads.Add(1)
	return nil, nil
}

func (c *reposClient) PullRequestState(ctx context.Context, _, name string, n int) (github.PRState, error) {
	return c.repos[name].PullRequestState(ctx, "", name, n)
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
	// The update records no versions, so its key moves with its head and
	// each push looks like an update not reviewed yet.
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rv := &fakeReviewer{errs: map[int][]error{1: {moved, moved, moved}}}
	rv.onReview = func(review.Request) {
		pr := &c.repo.PullRequests[0]
		pr.HeadOID = fmt.Sprintf("pushed%d", len(rv.calls))
		pr.ChecksOID = pr.HeadOID
	}
	r := reviewRunner(c, rv, true)
	r.MaxReviews = 5
	res := r.RunRepo(context.Background(), repoCfg)
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

func TestCacheCorruptedMidRunIsAWarning(t *testing.T) {
	path := t.TempDir() + "/verdicts.json"
	cache, err := review.OpenCache(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file goes bad after the run opened the cache, as when another
	// process's write was cut short.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	r := reviewRunner(c, &fakeReviewer{}, true)
	r.Verdicts = cache
	res := r.RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(c.merges) != "[1]" || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "unreadable") {
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

func TestMergeThatWentThroughButStillReadsOpenIsReportedMerged(t *testing.T) {
	// The merge times out but goes through; GitHub still shows the PR open
	// on the next read, so the retry gets a 405.
	fc := &fakeClient{
		repo:         testutil.Repo(testutil.PR(1)),
		mergeErrs:    map[int][]error{1: {&api.HTTPError{StatusCode: 502}}},
		mergedAnyway: map[int]bool{1: true},
		lag:          1,
	}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(fc.merges) != "[1]" || len(res.Events) != 1 || res.Events[0].Kind != EventMerged || res.Failed() {
		t.Fatalf("merges %v, events %+v", fc.merges, res.Events)
	}
	if !strings.Contains(res.Events[0].Detail, "after HTTP 502") {
		t.Fatalf("detail %q should name the first failure", res.Events[0].Detail)
	}
}

func TestTimedOutMergeRefusedOnRetryIsAFailure(t *testing.T) {
	notMergeable := &api.HTTPError{StatusCode: 405, Message: "Pull Request is not mergeable"}
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {&api.HTTPError{StatusCode: 504}, notMergeable}}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 0 || len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed || !res.Failed() {
		t.Fatalf("merges %v, events %+v", fc.merges, res.Events)
	}
	if !strings.Contains(res.Events[0].Detail, "not mergeable") {
		t.Fatalf("detail %q", res.Events[0].Detail)
	}
}

func TestTimedOutMergeIsReportedWhenTheRepoCannotBeReadAgain(t *testing.T) {
	bad := &api.HTTPError{StatusCode: 502}
	fc := &fakeClient{
		repo:                testutil.Repo(testutil.PR(1)),
		mergeErrs:           map[int][]error{1: {&api.HTTPError{StatusCode: 504}}},
		mergedAnyway:        map[int]bool{1: true},
		fetchErrsAfterMerge: []error{bad, bad},
		stateErrs:           []error{bad, bad},
	}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if res.Error == "" || len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed {
		t.Fatalf("error %q, events %+v", res.Error, res.Events)
	}
	if !strings.Contains(res.Events[0].Detail, "may have merged after HTTP 504") {
		t.Fatalf("detail %q", res.Events[0].Detail)
	}

	// GitHub can still say whether the PR merged when the repository
	// cannot be read.
	fc = &fakeClient{
		repo:                testutil.Repo(testutil.PR(1)),
		mergeErrs:           map[int][]error{1: {&api.HTTPError{StatusCode: 504}}},
		mergedAnyway:        map[int]bool{1: true},
		fetchErrsAfterMerge: []error{bad, bad},
	}
	res = newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if res.Error == "" || len(res.Events) != 1 || res.Events[0].Kind != EventMerged {
		t.Fatalf("error %q, events %+v", res.Error, res.Events)
	}
}

func TestTimedOutMergeOfAPRThatTurnsUnmergeableIsReported(t *testing.T) {
	// The merge times out; GitHub still shows the PR open, its
	// mergeability being recomputed, until the pass ends.
	fc := &fakeClient{
		repo:              testutil.Repo(testutil.PR(1)),
		mergeErrs:         map[int][]error{1: {&api.HTTPError{StatusCode: 502}}},
		mergedAnyway:      map[int]bool{1: true},
		lag:               20,
		unknownAfterMerge: 20,
	}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(fc.merges) != "[1]" || len(res.Events) != 1 || res.Events[0].Kind != EventMerged || res.Failed() {
		t.Fatalf("merges %v, events %+v", fc.merges, res.Events)
	}

	// The same, but the merge did not go through.
	fc = &fakeClient{
		repo:              testutil.Repo(testutil.PR(1)),
		mergeErrs:         map[int][]error{1: {&api.HTTPError{StatusCode: 502}}},
		unknownAfterMerge: 20,
	}
	res = newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 0 || len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed || !res.Failed() {
		t.Fatalf("merges %v, events %+v", fc.merges, res.Events)
	}
	if !strings.Contains(res.Events[0].Detail, "not merged after HTTP 502") {
		t.Fatalf("detail %q", res.Events[0].Detail)
	}
}

func TestTimedOutMergeOfAPRClosedMeanwhileIsAFailure(t *testing.T) {
	fc := &fakeClient{
		repo:         testutil.Repo(testutil.PR(1)),
		mergeErrs:    map[int][]error{1: {&api.HTTPError{StatusCode: 504}}},
		closedAnyway: map[int]bool{1: true},
	}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 0 || len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed || !res.Failed() {
		t.Fatalf("merges %v, events %+v; a PR closed unmerged is no merge", fc.merges, res.Events)
	}
	if !strings.Contains(res.Events[0].Detail, "closed without being merged, after HTTP 504") {
		t.Fatalf("detail %q", res.Events[0].Detail)
	}
}

func TestRunWarningsGoToTheFirstResult(t *testing.T) {
	c := &reposClient{repos: map[string]*fakeClient{"a": {repo: testutil.Repo()}, "b": {repo: testutil.Repo()}}}
	r := newRunner(c, false)
	r.Warn("verdicts.json was unreadable")
	results := r.Run(context.Background(), []config.Repo{{Owner: "o", Name: "a"}, {Owner: "o", Name: "b"}}, nil)
	if fmt.Sprint(results[0].Warnings) != "[verdicts.json was unreadable]" || len(results[1].Warnings) != 0 {
		t.Fatalf("warnings %q, %q", results[0].Warnings, results[1].Warnings)
	}
}

func TestTimedOutRebaseRequestIsAWarning(t *testing.T) {
	pr := testutil.PR(1)
	pr.MergeState = "BEHIND"
	fc := &fakeClient{repo: testutil.Repo(pr), commentErrs: map[int]error{1: &api.HTTPError{StatusCode: 504}}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(res.Events) != 1 || res.Events[0].Kind != EventRebaseRequested || !strings.Contains(res.Events[0].Detail, "may not have been posted") || res.Failed() {
		t.Fatalf("events %+v", res.Events)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "#1: the rebase request may not have been posted") {
		t.Fatalf("warnings %q", res.Warnings)
	}

	fc = &fakeClient{repo: testutil.Repo(pr), commentErrs: map[int]error{1: &api.HTTPError{StatusCode: 403, Message: "Resource not accessible"}}}
	res = newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(res.Events) != 1 || res.Events[0].Kind != EventRebaseFailed || !res.Failed() {
		t.Fatalf("events %+v; a refused comment is a failure", res.Events)
	}
}

func TestFailedReviewHasNoNullConcerns(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	res := reviewRunner(c, &fakeReviewer{fail: map[int]bool{1: true}}, true).RunRepo(context.Background(), repoCfg)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") || !strings.Contains(string(b), `"concerns":[]`) {
		t.Fatalf("json = %s", b)
	}
}

func TestRefusedReviewDoesNotUseUpTheLimit(t *testing.T) {
	// With a limit of one: o/a's review starts, o/b is refused meanwhile,
	// then o/a's head moves, which gives its review back. Its new head
	// must still get that review.
	c := &reposClient{repos: map[string]*fakeClient{
		"a": {repo: testutil.Repo(testutil.PR(1, "lib:major"))},
		"b": {repo: testutil.Repo(testutil.PR(2, "lib:major"))},
	}}
	moved := fmt.Errorf("checkout: %w", review.ErrHeadMoved)
	var r *Runner
	var refused RepoResult
	rv := &fakeReviewer{errs: map[int][]error{1: {moved}}}
	rv.onReview = func(req review.Request) {
		if len(rv.calls) == 1 {
			refused = r.RunRepo(context.Background(), config.Repo{Owner: "o", Name: "b", Policy: config.Builtin})
		}
	}
	r = reviewRunner(c, rv, false)
	r.MaxReviews = 1
	res := r.RunRepo(context.Background(), config.Repo{Owner: "o", Name: "a", Policy: config.Builtin})
	if fmt.Sprint(rv.calls) != "[1 1]" || res.Decisions[0].Action != policy.ActionMerge {
		t.Fatalf("reviews %v, decision %+v", rv.calls, res.Decisions[0])
	}
	if d := refused.Decisions[0]; !strings.Contains(d.Reason, "review limit of 1 reached") {
		t.Fatalf("o/b decision %+v", d)
	}
}

func TestLongRateLimitStopsLaterWrites(t *testing.T) {
	limited := &api.HTTPError{StatusCode: 403, Message: "secondary rate limit", Headers: map[string][]string{"Retry-After": {"600"}}}
	behind := testutil.PR(3)
	behind.MergeState = "BEHIND"
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2), behind, testutil.PR(4)), mergeErrs: map[int][]error{1: {limited}}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 0 || len(fc.comments) != 0 {
		t.Fatalf("merges %v, comments %v; nothing may be sent through a rate limit", fc.merges, fc.comments)
	}
	if len(res.Events) != 3 || !strings.Contains(res.Events[0].Detail, "not waiting up to 10m0s") {
		t.Fatalf("events %+v", res.Events)
	}
	for _, ev := range res.Events[1:] {
		if ev.Kind != EventMergeFailed || !strings.HasPrefix(ev.Detail, "not sent") {
			t.Fatalf("events %+v", res.Events)
		}
	}
	if fc.fetches != 1 {
		t.Fatalf("%d fetches; merges that cannot be sent need no fresh read", fc.fetches)
	}
}

func TestRateLimitThatRefusesAgainStopsLaterWrites(t *testing.T) {
	limited := &api.HTTPError{StatusCode: 403, Message: "secondary rate limit", Headers: map[string][]string{"Retry-After": {"20"}}}
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2)), mergeErrs: map[int][]error{1: {limited, limited}}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(fc.merges) != 0 || len(res.Events) != 2 || !strings.Contains(res.Events[0].Detail, "rate-limited again after waiting 20s") || !strings.HasPrefix(res.Events[1].Detail, "not sent") {
		t.Fatalf("merges %v, events %+v", fc.merges, res.Events)
	}
	if strings.Contains(res.Events[0].Detail, "not waiting") {
		t.Fatalf("detail %q; it did wait", res.Events[0].Detail)
	}
}

// cancellingClient cancels the run while a merge is in flight, which then
// fails with err, or with the cancellation when err is nil.
type cancellingClient struct {
	*fakeClient
	cancel context.CancelFunc
	err    error
}

func (c *cancellingClient) Merge(context.Context, string, string, int, string, string) error {
	c.cancel()
	if c.err != nil {
		return c.err
	}
	return fmt.Errorf("Put \"https://api.github.com/repos/o/r/pulls/1/merge\": %w", context.Canceled)
}

func TestMergeInterruptedInFlightMayHaveMerged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &cancellingClient{fakeClient: &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2))}, cancel: cancel}
	res := newRunner(c, true).RunRepo(ctx, repoCfg)
	if len(res.Events) == 0 || res.Events[0].Detail != "interrupted; may have merged" {
		t.Fatalf("events %+v", res.Events)
	}
	for _, ev := range res.Events[1:] {
		if strings.Contains(ev.Detail, "may have merged") {
			t.Errorf("event %+v: a merge never sent cannot have merged", ev)
		}
	}
}

func TestMergeAnsweredAsTheRunIsInterrupted(t *testing.T) {
	for err, want := range map[error]string{
		// GitHub refused it: it did not merge.
		&api.HTTPError{StatusCode: 422, Message: "Head branch was modified"}:      "interrupted",
		&api.HTTPError{StatusCode: 405, Message: "Pull Request is not mergeable"}: "interrupted",
		// GitHub gave up on it, which may have merged it all the same.
		&api.HTTPError{StatusCode: 502}: "interrupted; may have merged",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		c := &cancellingClient{fakeClient: &fakeClient{repo: testutil.Repo(testutil.PR(1))}, cancel: cancel, err: err}
		res := newRunner(c, true).RunRepo(ctx, repoCfg)
		cancel()
		if len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed || res.Events[0].Detail != want {
			t.Errorf("%v: events %+v, want detail %q", err, res.Events, want)
		}
	}
}

// interruptingClient cancels the run as the repository is read for the
// cancelAt-th time, once the read itself has succeeded.
type interruptingClient struct {
	*fakeClient
	cancel   context.CancelFunc
	cancelAt int
}

func (c *interruptingClient) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	repo, err := c.fakeClient.FetchRepo(ctx, owner, name)
	if c.fetches == c.cancelAt {
		c.cancel()
	}
	return repo, err
}

func TestTimedOutMergeInterruptedBeforeItsRetryMayHaveMerged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {&api.HTTPError{StatusCode: 504}}}, mergedAnyway: map[int]bool{1: true}, lag: 5}
	res := newRunner(&interruptingClient{fc, cancel, 2}, true).RunRepo(ctx, repoCfg)
	if len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed || !strings.HasPrefix(res.Events[0].Detail, "interrupted; may have merged after HTTP 504") {
		t.Fatalf("events %+v", res.Events)
	}
}

// ctxStateClient fails to say a PR's state once the run is interrupted,
// as the real client does.
type ctxStateClient struct{ *fakeClient }

func (c ctxStateClient) PullRequestState(ctx context.Context, owner, name string, number int) (github.PRState, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.fakeClient.PullRequestState(ctx, owner, name, number)
}

func TestTimedOutMergeInterruptedInThePauseBeforeItsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {&api.HTTPError{StatusCode: 504}}}}
	r := newRunner(ctxStateClient{fc}, true)
	r.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	res := r.RunRepo(ctx, repoCfg)
	if len(res.Events) != 1 || !strings.HasPrefix(res.Events[0].Detail, "interrupted; may have merged after HTTP 504") {
		t.Fatalf("events %+v", res.Events)
	}
}

// pendingFirstClient shows PR 1's checks still running in the first read.
type pendingFirstClient struct{ *fakeClient }

func (c pendingFirstClient) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	repo, err := c.fakeClient.FetchRepo(ctx, owner, name)
	if repo != nil && c.fetches == 1 {
		for i, pr := range repo.PullRequests {
			if pr.Number == 1 {
				pr.Checks = []github.Check{{Name: "build", State: github.CheckPending}}
				repo.PullRequests[i] = pr
			}
		}
	}
	return repo, err
}

func TestRetryCutOffByBlockedWritesIsReportedOnce(t *testing.T) {
	limited := &api.HTTPError{StatusCode: 403, Message: "API rate limit exceeded", Headers: map[string][]string{"Retry-After": {"3600"}}}
	for _, merged := range []bool{false, true} {
		fc := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2)), mergeErrs: map[int][]error{1: {limited}, 2: {&api.HTTPError{StatusCode: 504}}}, mergedAnyway: map[int]bool{2: merged}}
		res := newRunner(pendingFirstClient{fc}, true).RunRepo(context.Background(), repoCfg)
		var two []Event
		for _, ev := range res.Events {
			if ev.Number == 2 {
				two = append(two, ev)
			}
		}
		switch {
		case len(two) != 1:
			t.Errorf("merged %v: PR 2 reported %d times: %+v", merged, len(two), two)
		case merged && two[0].Kind != EventMerged:
			t.Errorf("PR 2 merged by its first try, reported as %+v", two[0])
		case !merged && (two[0].Kind != EventMergeFailed || !strings.Contains(two[0].Detail, "after HTTP 504") || !strings.Contains(two[0].Detail, "retry not sent")):
			t.Errorf("PR 2 reported as %+v", two[0])
		}
	}
}

func TestRefusedMergeIsAPlainFailureWhenGitHubCannotSay(t *testing.T) {
	bad := &api.HTTPError{StatusCode: 502}
	notMergeable := &api.HTTPError{StatusCode: 405, Message: "Base branch was modified"}
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {notMergeable}}, unknownAfterMerge: 20, stateErrs: []error{bad, bad}}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed || !res.Failed() {
		t.Fatalf("events %+v", res.Events)
	}
	if d := res.Events[0].Detail; strings.Contains(d, "may have merged") || !strings.Contains(d, "HTTP 405") {
		t.Fatalf("detail %q; a refused merge did not go through", d)
	}
}

func TestConnectionResetMergeMayHaveGoneThrough(t *testing.T) {
	reset := &url.Error{Op: "Put", URL: "https://api.github.com/repos/o/r/pulls/1/merge", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
	for _, err := range []error{reset, &url.Error{Op: "Put", URL: "u", Err: io.EOF}} {
		fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {err}}, mergedAnyway: map[int]bool{1: true}}
		res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
		if fmt.Sprint(fc.merges) != "[1]" || len(res.Events) != 1 || res.Events[0].Kind != EventMerged || res.Failed() {
			t.Fatalf("%v: merges %v, events %+v", err, fc.merges, res.Events)
		}
	}
}

func TestRunnerWithoutNewDoesNotPanic(t *testing.T) {
	notMergeable := &api.HTTPError{StatusCode: 405, Message: "Base branch was modified"}
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {notMergeable}}}
	r := &Runner{Client: fc, Viewer: "me", Apply: true, PollAttempts: 1}
	if res := r.RunRepo(context.Background(), repoCfg); fmt.Sprint(fc.merges) != "[1]" || res.Failed() {
		t.Fatalf("merges %v, result %+v", fc.merges, res)
	}
}

func TestResultJSONShape(t *testing.T) {
	pr := testutil.PR(1)
	pr.Commits[0].Message = "bump"
	res := newRunner(&fakeClient{repo: testutil.Repo(pr)}, false).RunRepo(context.Background(), repoCfg)
	for _, r := range []RepoResult{res, {Repo: "o/gone", Error: "not found", Decisions: []policy.Decision{{Number: 2}}}} {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"permission", "merge_method"} {
			if _, ok := got[key].(string); !ok {
				t.Errorf("%s: %s = %v, want a string", b, key, got[key])
			}
		}
		for _, key := range []string{"decisions", "findings", "events", "warnings"} {
			if _, ok := got[key].([]any); !ok {
				t.Errorf("%s: %s = %v, want a list", b, key, got[key])
			}
		}
		d := got["decisions"].([]any)[0].(map[string]any)
		for _, key := range []string{"dependencies", "failed_checks"} {
			if _, ok := d[key].([]any); !ok {
				t.Errorf("%s: decision %s = %v, want a list", b, key, d[key])
			}
		}
	}

	dep := policy.Decision{Dependencies: []dependabot.Dependency{{Name: "lib"}}}
	b, err := json.Marshal(RepoResult{Decisions: []policy.Decision{dep}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"dependencies":[{"name":"lib","version":"","type":"","group":"","update_type":"unknown"}]`) {
		t.Fatalf("json = %s", b)
	}
}

func TestRefusedMergeOfAPRThatTurnsUnmergeableIsReported(t *testing.T) {
	notMergeable := &api.HTTPError{StatusCode: 405, Message: "Base branch was modified"}
	fc := &fakeClient{repo: testutil.Repo(testutil.PR(1)), mergeErrs: map[int][]error{1: {notMergeable}}, unknownAfterMerge: 20}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if len(res.Events) != 1 || res.Events[0].Kind != EventMergeFailed || !res.Failed() {
		t.Fatalf("events %+v; a refused merge must not go unreported", res.Events)
	}
	if !strings.Contains(res.Events[0].Detail, "not merged after HTTP 405") {
		t.Fatalf("detail %q", res.Events[0].Detail)
	}
}

func TestRejectionInThisRunLapsesWhenTheHeadMoves(t *testing.T) {
	// With a version recorded, the update keeps its key across a rebase.
	versioned := testutil.PR(1, "lib:major")
	versioned.Commits[0].Message = strings.Replace(versioned.Commits[0].Message, "  dependency-type:", "  dependency-version: 2.0.0\n  dependency-type:", 1)
	c := &fakeClient{repo: testutil.Repo(versioned)}
	rv := &fakeReviewer{verdicts: map[int]policy.Verdict{1: {Summary: "breaks the build"}}}
	rv.onReview = func(review.Request) {
		if len(rv.calls) == 1 {
			// Dependabot rebases onto a fix while the first review runs.
			pr := &c.repo.PullRequests[0]
			pr.HeadOID, pr.ChecksOID = "rebased", "rebased"
			return
		}
		delete(rv.verdicts, 1)
	}
	res := reviewRunner(c, rv, true).RunRepo(context.Background(), repoCfg)
	if fmt.Sprint(rv.calls) != "[1 1]" || fmt.Sprint(c.merges) != "[1]" {
		t.Fatalf("reviews %v, merges %v, decisions %+v", rv.calls, c.merges, res.Decisions)
	}
}

func TestRepoFilesSetThePolicy(t *testing.T) {
	behind := testutil.PR(2)
	behind.MergeState = "BEHIND"
	fc := &fakeClient{
		repo: testutil.Repo(testutil.PR(1), behind),
		files: map[string]string{
			"o/.github:deps-merger.yml":   "dependabot_commands: false\n",
			"o/r:.github/deps-merger.yml": "mode: report\n",
		},
	}
	res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	// Report mode: nothing is written, though the run applies.
	if len(fc.merges) != 0 || len(fc.comments) != 0 {
		t.Fatalf("merges = %v, comments = %v", fc.merges, fc.comments)
	}
	if res.Policy.Mode != config.ModeReport || res.Policy.DependabotCommands {
		t.Errorf("policy = %+v", res.Policy)
	}
	if want := []string{"o/.github:deps-merger.yml", ".github/deps-merger.yml"}; !slices.Equal(res.ConfigFiles, want) {
		t.Errorf("config files = %v, want %v", res.ConfigFiles, want)
	}
	if res.Decisions[0].Action != policy.ActionMerge {
		t.Errorf("#1 = %s %q", res.Decisions[0].Action, res.Decisions[0].Reason)
	}
}

func TestBadOrUnreadableRepoFileFailsTheRepo(t *testing.T) {
	for name, fc := range map[string]*fakeClient{
		"invalid":    {files: map[string]string{"o/r:.github/deps-merger.yml": "max_update_type: huge\n"}},
		"unreadable": {fileErr: errors.New("boom")},
	} {
		fc.repo = testutil.Repo(testutil.PR(1))
		res := newRunner(fc, true).RunRepo(context.Background(), repoCfg)
		if res.Error == "" || len(fc.merges) != 0 || len(res.Decisions) != 0 {
			t.Errorf("%s: error %q, merges %v, decisions %d", name, res.Error, fc.merges, len(res.Decisions))
		}
	}
}

func TestOrgFileIsReadOncePerOwner(t *testing.T) {
	c := &reposClient{repos: map[string]*fakeClient{}}
	for _, name := range []string{"a", "b", "c"} {
		c.repos[name] = &fakeClient{repo: testutil.Repo()}
	}
	r := newRunner(c, false)
	repos := []config.Repo{
		{Owner: "o", Name: "a", Policy: config.Builtin},
		{Owner: "O", Name: "b", Policy: config.Builtin},
		{Owner: "p", Name: "c", Policy: config.Builtin},
	}
	r.Run(context.Background(), repos, nil)
	// One org file for each of o and p, and each repo's own.
	if got := c.fileReads.Load(); got != 5 {
		t.Errorf("file reads = %d, want 5", got)
	}
}
