package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/review"
)

// remote is a git repository holding the heads of PRs 1 to n as
// refs/pull/N/head, and returns it with each head's commit.
func remote(t *testing.T, n int) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	var heads []string
	for i := 1; i <= n; i++ {
		os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies": {"lib": "^`+string(rune('0'+i))+`.0.0"}}`), 0o644)
		git("add", "-A")
		git("commit", "-q", "-m", "bump lib")
		head := git("rev-parse", "HEAD")
		git("update-ref", "refs/pull/"+string(rune('0'+i))+"/head", head)
		heads = append(heads, head)
	}
	return dir, heads
}

// major is a held major update whose head is head.
func major(n int, head string) github.PullRequest {
	p := pr(n)
	p.Commits[0].Message = strings.Replace(p.Commits[0].Message, "semver-patch", "semver-major", 1)
	p.HeadOID, p.ChecksOID = head, head
	p.Commits[0].OID = head
	return p
}

// agent answers every review with decision, and counts them.
type agent struct {
	mu       sync.Mutex
	decision string
	prompts  []string
	// delay is how long each review takes.
	delay time.Duration
}

func (a *agent) build(usage func(review.Usage)) review.Agent { return agentRun{a, usage} }

type agentRun struct {
	a     *agent
	usage func(review.Usage)
}

func (r agentRun) Name() string { return "fake" }

func (r agentRun) Run(_ context.Context, dir, prompt string) (review.Answer, error) {
	r.a.mu.Lock()
	r.a.prompts = append(r.a.prompts, prompt)
	r.a.mu.Unlock()
	time.Sleep(r.a.delay)
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return review.Answer{}, err
	}
	r.usage(review.Usage{Requests: 2, PromptTokens: 1200, CompletionTokens: 80})
	return review.Answer{Decision: r.a.decision, Summary: "lib 2 is fine here", BreakingChanges: []string{}, Concerns: []string{}}, nil
}

func withReview(t *testing.T, s *Service, a *agent, repo string, perDay int) {
	s.Review = &Review{
		Agent: a.build, Model: "qwen-test", PerDay: perDay, Timeout: time.Minute, Dir: t.TempDir(),
		CheckoutURL: func(string, string) string { return "file://" + repo },
	}
}

func TestReviewApprovesAMajorThatThenMerges(t *testing.T) {
	repo, heads := remote(t, 1)
	s, p := setup(t, true, "mode: merge\nagent_review: true\n", major(1, heads[0]))
	a := &agent{decision: "approve"}
	withReview(t, s, a, repo, 10)
	ctx := context.Background()
	s.Store.InstallationAdded(ctx, 1, "o", time.Now())
	s.RunRepo(ctx, 1, "o/r", "test")
	if len(a.prompts) != 1 || !strings.Contains(a.prompts[0], "Pull request #1") {
		t.Fatalf("reviews: %d", len(a.prompts))
	}
	if !slices.Equal(p.engine.merged, []int{1}) {
		t.Fatalf("merged %v", p.engine.merged)
	}
	ms, _ := s.Store.Metrics(ctx, time.Now())
	if len(ms) != 1 || ms[0].Reviews != 1 || ms[0].ReviewsApproved != 1 || ms[0].PromptTokens != 1200 {
		t.Fatalf("metrics = %+v", ms)
	}
}

func TestAppMergesAreTimedWhenTheyHappen(t *testing.T) {
	repo, heads := remote(t, 2)
	start := time.Now()
	p1, p2 := major(1, heads[0]), major(2, heads[1])
	p1.CreatedAt, p2.CreatedAt = start, start
	s, p := setup(t, true, "mode: merge\nagent_review: true\n", p1, p2)
	a := &agent{decision: "approve", delay: 300 * time.Millisecond}
	withReview(t, s, a, repo, 10)
	ctx := context.Background()
	s.Store.InstallationAdded(ctx, 1, "o", start)
	s.RunRepo(ctx, 1, "o/r", "test")
	took := time.Since(start)
	if !slices.Equal(p.engine.merged, []int{1, 2}) {
		t.Fatalf("merged %v", p.engine.merged)
	}
	// #1 merged after one review, #2 after two: about three quarters of
	// the run in the median. Timed at the run's end, it would be all of it.
	ms, _ := s.Store.Metrics(ctx, time.Now())
	if got := ms[0].MergeTimeApp; got >= took*7/8 {
		t.Fatalf("median merge time %v of a %v run", got, took)
	}
}

func TestNoReviewWhileTheModelIsUnavailable(t *testing.T) {
	repo, heads := remote(t, 1)
	s, p := setup(t, true, "mode: merge\nagent_review: true\n", major(1, heads[0]))
	a := &agent{decision: "approve"}
	withReview(t, s, a, repo, 10)
	s.Review.Available = func(context.Context) error { return errors.New("connection refused") }
	ctx := context.Background()
	s.RunRepo(ctx, 1, "o/r", "test")
	if len(a.prompts) != 0 || len(p.engine.merged) != 0 {
		t.Fatalf("reviews %d, merged %v", len(a.prompts), p.engine.merged)
	}
	if n, _ := s.Store.ReviewsSince(ctx, 1, time.Time{}); n != 0 {
		t.Fatalf("%d reviews recorded", n)
	}
	if body := p.issues.Get(1).Body; !strings.Contains(body, "model is unavailable") {
		t.Fatalf("issue does not say the model is unavailable:\n%s", body)
	}
	// Back up: the next run reviews it.
	s.Review.Available = func(context.Context) error { return nil }
	s.RunRepo(ctx, 1, "o/r", "test")
	if len(a.prompts) != 1 || !slices.Equal(p.engine.merged, []int{1}) {
		t.Fatalf("reviews %d, merged %v", len(a.prompts), p.engine.merged)
	}
}

func TestKnownVerdictsStandWhileTheModelIsUnavailable(t *testing.T) {
	repo, heads := remote(t, 1)
	s, p := setup(t, true, "mode: merge\nagent_review: true\n", major(1, heads[0]))
	a := &agent{decision: "reject"}
	withReview(t, s, a, repo, 10)
	ctx := context.Background()
	s.RunRepo(ctx, 1, "o/r", "test")
	before := p.issues.Get(1).Body
	s.Review.Available = func(context.Context) error { return errors.New("connection refused") }
	s.Review.Model = "replacement-model"
	s.RunRepo(ctx, 1, "o/r", "test")
	if len(a.prompts) != 1 || p.issues.Get(1).Body != before || !strings.Contains(before, "rejected by qwen-test") {
		t.Fatalf("reviews %d; issue before:\n%s\nafter:\n%s", len(a.prompts), before, p.issues.Get(1).Body)
	}
}

func TestReportIssueNamesAgentOnCachedApproval(t *testing.T) {
	repo, heads := remote(t, 1)
	s, p := setup(t, false, "agent_review: true\n", major(1, heads[0]))
	a := &agent{decision: "approve"}
	withReview(t, s, a, repo, 10)
	ctx := context.Background()
	s.RunRepo(ctx, 1, "o/r", "test")
	before := p.issues.Get(1).Body
	if !strings.Contains(before, "approved by qwen-test") {
		t.Fatalf("issue does not name the reviewer:\n%s", before)
	}
	s.Review.Model = "replacement-model"
	s.Review.Available = func(context.Context) error { return errors.New("connection refused") }
	s.RunRepo(ctx, 1, "o/r", "test")
	if len(a.prompts) != 1 || p.issues.Get(1).Body != before {
		t.Fatalf("cached approval changed reviewer in issue:\nbefore:\n%s\nafter:\n%s", before, p.issues.Get(1).Body)
	}
}

func TestReviewRejectionHoldsThePRAndIsCached(t *testing.T) {
	repo, heads := remote(t, 1)
	s, p := setup(t, true, "mode: merge\nagent_review: true\n", major(1, heads[0]))
	a := &agent{decision: "reject"}
	withReview(t, s, a, repo, 10)
	ctx := context.Background()
	s.RunRepo(ctx, 1, "o/r", "test")
	s.RunRepo(ctx, 1, "o/r", "test")
	if len(p.engine.merged) != 0 {
		t.Fatalf("merged %v", p.engine.merged)
	}
	// The second run reuses the verdict: the PR's head did not move.
	if len(a.prompts) != 1 {
		t.Fatalf("reviewed %d times", len(a.prompts))
	}
	if body := p.issues.Get(1).Body; !strings.Contains(body, "rejected by qwen-test") || !strings.Contains(body, "lib 2 is fine here") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestReviewsStopAtTheDailyAllowance(t *testing.T) {
	repo, heads := remote(t, 3)
	s, _ := setup(t, false, "agent_review: true\n", major(1, heads[0]), major(2, heads[1]), major(3, heads[2]))
	a := &agent{decision: "reject"}
	withReview(t, s, a, repo, 2)
	ctx := context.Background()
	s.RunRepo(ctx, 1, "o/r", "test")
	s.RunRepo(ctx, 1, "o/r", "test")
	if len(a.prompts) != 2 {
		t.Fatalf("reviewed %d times, allowance 2", len(a.prompts))
	}
}

func TestNoReviewUnlessTheRepositoryAsks(t *testing.T) {
	repo, heads := remote(t, 1)
	s, p := setup(t, true, "mode: merge\n", major(1, heads[0]))
	a := &agent{decision: "approve"}
	withReview(t, s, a, repo, 10)
	s.RunRepo(context.Background(), 1, "o/r", "test")
	if len(a.prompts) != 0 || len(p.engine.merged) != 0 {
		t.Fatalf("reviewed %d, merged %v", len(a.prompts), p.engine.merged)
	}
}

func TestForgetRemovesCheckoutsAndVerdicts(t *testing.T) {
	repo, heads := remote(t, 1)
	s, _ := setup(t, false, "agent_review: true\n", major(1, heads[0]))
	withReview(t, s, &agent{decision: "reject"}, repo, 10)
	ctx := context.Background()
	s.RunRepo(ctx, 1, "o/r", "test")
	for _, p := range []string{s.Review.checkouts(1), s.Review.verdicts(1)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing before forgetting: %v", p, err)
		}
	}
	if err := s.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Review.checkouts(1), s.Review.verdicts(1)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept after forgetting", p)
		}
	}
}
