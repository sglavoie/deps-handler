package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-handler/internal/jobs"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

type limitedEngine struct {
	github.Client
	mergeErr, commentErr error
	merges               []string
}

func (e *limitedEngine) Merge(ctx context.Context, owner, name string, number int, method, head string) error {
	e.merges = append(e.merges, owner+"/"+name)
	if e.mergeErr != nil {
		return e.mergeErr
	}
	return e.Client.Merge(ctx, owner, name, number, method, head)
}

func (e *limitedEngine) Comment(ctx context.Context, owner, name string, number int, body string) error {
	if e.commentErr != nil {
		return e.commentErr
	}
	return e.Client.Comment(ctx, owner, name, number, body)
}

type limitedPlatform struct {
	*platform
	client github.Client
}

func (p limitedPlatform) Engine(context.Context, int64) (github.Client, error) { return p.client, nil }

type limitedRepoPlatform struct {
	*platform
	err   error
	calls map[int64]int
}

func (p *limitedRepoPlatform) Repos(ctx context.Context, installation int64) ([]string, error) {
	p.calls[installation]++
	if p.err != nil {
		return nil, p.err
	}
	return p.platform.Repos(ctx, installation)
}

func TestRepositoryListingsShareInstallationCooldown(t *testing.T) {
	for _, source := range []string{"repository list", "engine"} {
		t.Run(source, func(t *testing.T) {
			s, p := setup(t, false, "", pr(1))
			ctx := context.Background()
			limited := &limitedRepoPlatform{platform: p, calls: map[int64]int{}}
			s.Platform = limited
			limit := &api.HTTPError{StatusCode: 429, Headers: http.Header{"Retry-After": {"600"}}}
			wantCalls := 0
			if source == "repository list" {
				limited.err = limit
				if err := s.Run(ctx, jobs.Job{Installation: 1}); err == nil {
					t.Fatal("expected repository listing rate limit")
				}
				limited.err = nil
				wantCalls = 1
			} else {
				s.gate(1).observeRateLimit(limit)
			}
			for _, retry := range []struct {
				name string
				run  func() error
			}{
				{"queued installation retry", func() error { return s.Run(ctx, jobs.Job{Installation: 1}) }},
				{"once", func() error { return s.Once(ctx, nil) }},
				{"repository webhook", func() error { return s.SyncRepositoryWebhook(ctx, 1, "o/r") }},
				{"repository run", func() error { return s.RunRepo(ctx, 1, "o/r", "retry") }},
			} {
				if err := retry.run(); err == nil || !strings.Contains(err.Error(), "rate limit") {
					t.Errorf("%s ignored cooldown: %v", retry.name, err)
				}
				if limited.calls[1] != wantCalls {
					t.Errorf("%s sent a repository list request during cooldown: calls = %d, want %d", retry.name, limited.calls[1], wantCalls)
				}
			}
			if calls := p.issues.TakeCalls(); len(calls) != 0 {
				t.Fatalf("cooldown allowed issue requests: %v", calls)
			}
			if err := s.SyncRepositoryWebhook(ctx, 2, "o/r"); err != nil || limited.calls[2] != 1 {
				t.Fatalf("other installation was blocked: calls = %d, err = %v", limited.calls[2], err)
			}
			expireRateLimit(s, 1)
			if err := s.SyncRepositoryWebhook(ctx, 1, "o/r"); err != nil || limited.calls[1] != wantCalls+1 {
				t.Fatalf("expired cooldown still blocked discovery: calls = %d, err = %v", limited.calls[1], err)
			}
		})
	}
}

func expireRateLimit(s *Service, installation int64) {
	g := s.gate(installation)
	g.rateMu.Lock()
	g.retryAt = time.Now().Add(-time.Second)
	g.rateMu.Unlock()
}

func TestMergeRateLimitCoversIssuesAndOtherRepositories(t *testing.T) {
	for _, once := range []bool{false, true} {
		t.Run(fmt.Sprintf("once=%t", once), func(t *testing.T) {
			s, p := setup(t, true, "mode: merge\n", pr(1))
			p.repos = []string{"o/r", "o/second"}
			p.engine.files["o/second:.github/deps-handler.yml"] = "mode: merge\n"
			e := &limitedEngine{Client: p.engine, mergeErr: &api.HTTPError{
				StatusCode: 429, Headers: http.Header{"Retry-After": {"600"}},
			}}
			s.Platform = limitedPlatform{p, e}
			var err error
			if once {
				err = s.Once(context.Background(), nil)
			} else {
				err = s.Run(context.Background(), jobs.Job{Installation: 1, Trigger: "schedule"})
			}
			if err == nil || !strings.Contains(err.Error(), "rate limit") {
				t.Fatalf("run error = %v", err)
			}
			if !slices.Equal(e.merges, []string{"o/r"}) || len(p.issues.TakeCalls()) != 0 {
				t.Fatalf("requests passed through cooldown: merges %v; writes %v", e.merges, writes(t, s))
			}
			// A queued retry of a single repository must retain the cooldown.
			if err := s.RunRepo(context.Background(), 1, "o/r", "retry"); err == nil || len(e.merges) != 1 {
				t.Fatalf("retry ignored cooldown: %v; merges %v", err, e.merges)
			}
			// Another installation has its own quota and remains usable.
			e.mergeErr = nil
			if err := s.RunRepo(context.Background(), 2, "o/r", "other-installation"); err != nil {
				t.Fatal(err)
			}
			if len(e.merges) != 2 {
				t.Fatalf("other installation could not merge: %v", e.merges)
			}
			p.engine.prs = []github.PullRequest{pr(2)}
			expireRateLimit(s, 1)
			if err := s.RunRepo(context.Background(), 1, "o/r", "after-reset"); err != nil {
				t.Fatal(err)
			}
			if len(e.merges) != 3 || !slices.Equal(p.engine.merged, []int{1, 2}) {
				t.Fatalf("expired cooldown still blocked merges: %v", e.merges)
			}
		})
	}
}

func TestReviewCommentRateLimitBlocksMergesAndIssues(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	e := &limitedEngine{Client: p.engine, commentErr: &api.HTTPError{
		StatusCode: 403, Message: "secondary rate limit", Headers: http.Header{"Retry-After": {"600"}},
	}}
	c := &guarded{Client: e, s: s, gate: s.gate(1), installation: 1, repo: "o/r", log: s.Log}
	// Give the audit record a real run, as production does.
	var err error
	c.runID, err = s.Store.StartRun(context.Background(), 1, "o/r", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Comment(context.Background(), "o", "r", 1, runner.ReviewCommentPrefix+"test -->"); err == nil {
		t.Fatal("expected comment rate limit")
	}
	if err := c.Merge(context.Background(), "o", "r", 1, "squash", "sha1"); err == nil || len(e.merges) != 0 {
		t.Fatalf("merge after limited comment: %v; merges %v", err, e.merges)
	}
	if err := s.RunRepo(context.Background(), 1, "o/r", "retry"); err == nil || len(p.issues.TakeCalls()) != 0 {
		t.Fatalf("issue requests after limited comment: %v", err)
	}
}

func TestIssueRateLimitsCarryOverToEngine(t *testing.T) {
	for _, stage := range []string{"list", "create", "edit", "pin", "pin-graphql", "pin-graphql-secondary"} {
		t.Run(stage, func(t *testing.T) {
			s, p := setup(t, false, "", pr(1))
			if stage == "edit" {
				p.issues.Add(login, "Dependency health", "<!-- deps-handler:dependency-health -->\nold", "open")
			}
			var requests atomic.Int32
			var limited atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				limit := stage == "list" && r.Method == "GET" ||
					stage == "create" && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues") ||
					stage == "edit" && r.Method == "PATCH" ||
					strings.HasPrefix(stage, "pin") && r.URL.Path == "/graphql"
				if limit {
					limited.Store(true)
					if strings.HasPrefix(stage, "pin-graphql") {
						if stage == "pin-graphql-secondary" {
							w.Header().Set("Retry-After", "600")
						} else {
							w.Header().Set("X-Ratelimit-Remaining", "0")
							w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(time.Now().Add(10*time.Minute).Unix()))
						}
						fmt.Fprint(w, `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
						return
					}
					w.Header().Set("Retry-After", "600")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"message":"slow down: secondary rate limit"}`)
					return
				}
				p.issues.ServeHTTP(w, r)
			}))
			defer srv.Close()
			base := srv.URL + "/"
			var err error
			p.rest, err = gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
			if err != nil {
				t.Fatal(err)
			}
			err = s.RunRepo(context.Background(), 1, "o/r", "test")
			if !limited.Load() || (!strings.HasPrefix(stage, "pin") && (err == nil || !strings.Contains(err.Error(), "slow down"))) {
				t.Fatalf("rate limit or original error body was lost: limited %t; err %v", limited.Load(), err)
			}
			if strings.HasPrefix(stage, "pin") && err != nil {
				t.Fatalf("best-effort pin failed the confirmed issue creation: %v", err)
			}
			if delay, limited := github.RateLimited(s.gate(1).checkRateLimit()); !limited || delay < 9*time.Minute {
				t.Fatalf("issue rate limit did not preserve the requested cooldown: delay %v, limited %t", delay, limited)
			}
			before := requests.Load()
			s.AllowMerge = true
			p.engine.files["o/r:.github/deps-handler.yml"] = "mode: merge\n"
			if err := s.RunRepo(context.Background(), 1, "o/r", "retry"); err == nil {
				t.Fatal("engine ignored issue client's cooldown")
			}
			if len(p.engine.merged) != 0 || requests.Load() != before {
				t.Fatalf("requests passed through issue cooldown: merges %v; requests %d -> %d", p.engine.merged, before, requests.Load())
			}
		})
	}
}

func TestShortMergeRateLimitRetriesAfterCooldown(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	e := &limitedEngine{Client: p.engine, mergeErr: &api.HTTPError{
		StatusCode: 429, Headers: http.Header{"Retry-After": {"20"}},
	}}
	s.Platform = limitedPlatform{p, e}
	waits := 0
	s.Sleep = func(_ context.Context, d time.Duration) error {
		waits++
		if d != 20*time.Second {
			t.Fatalf("wait = %v, want 20s", d)
		}
		expireRateLimit(s, 1)
		e.mergeErr = nil
		return nil
	}
	if err := s.RunRepo(context.Background(), 1, "o/r", "test"); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || len(e.merges) != 2 || !slices.Equal(p.engine.merged, []int{1}) {
		t.Fatalf("short cooldown did not retry successfully: waits %d, attempts %v, merged %v", waits, e.merges, p.engine.merged)
	}
}

func TestIssueCooldownUnderstandsRetryAfterHeaders(t *testing.T) {
	for _, value := range []string{"600", time.Now().Add(10 * time.Minute).UTC().Format(http.TimeFormat), "18446744073709551615"} {
		t.Run(value, func(t *testing.T) {
			g := &installationGate{}
			resp := &http.Response{StatusCode: 403, Header: http.Header{"Retry-After": {value}},
				Body: http.NoBody}
			// A secondary-limit response with a documentation URL uses this
			// distinct error type, whose parsed RetryAfter can be incorrect.
			zero := time.Duration(0)
			g.observeRateLimit(fmt.Errorf("wrapped: %w", &gh.AbuseRateLimitError{Response: resp, RetryAfter: &zero}))
			if err := g.checkRateLimit(); err == nil {
				t.Fatal("missing cooldown")
			}
			if delay := time.Until(g.retryAt); delay < 9*time.Minute {
				t.Fatalf("shortened Retry-After %q to %v", value, delay)
			}
			previous := g.retryAt
			g.observeRateLimit(&api.HTTPError{StatusCode: 429, Headers: http.Header{"Retry-After": {"1"}}})
			if !g.retryAt.Equal(previous) {
				t.Fatal("a shorter pause replaced the existing cooldown")
			}
		})
	}
}

func TestIssuePermissionFailureDoesNotStartCooldown(t *testing.T) {
	g := &installationGate{}
	g.observeRateLimit(&gh.ErrorResponse{Response: &http.Response{StatusCode: 403}, Message: "Resource not accessible by integration"})
	if err := g.checkRateLimit(); err != nil {
		t.Fatalf("ordinary permission error started a cooldown: %v", err)
	}
}

func TestIssueCooldownKeepsParsedResetWithoutRawQuotaHeaders(t *testing.T) {
	pause := 10 * time.Minute
	for _, err := range []error{
		&gh.AbuseRateLimitError{Response: &http.Response{StatusCode: 403}, RetryAfter: &pause},
		&gh.RateLimitError{Response: &http.Response{StatusCode: 403}, Rate: gh.Rate{Reset: gh.Timestamp{Time: time.Now().Add(pause)}}},
	} {
		g := &installationGate{}
		g.observeRateLimit(err)
		if delay := time.Until(g.retryAt); delay < 9*time.Minute {
			t.Fatalf("lost parsed cooldown from %T: %v", err, delay)
		}
	}
}
