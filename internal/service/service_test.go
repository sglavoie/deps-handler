package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-merger/internal/app"
	"github.com/sglavoie/deps-merger/internal/ghfake"
	"github.com/sglavoie/deps-merger/internal/jobs"
	"github.com/sglavoie/deps-merger/internal/store"
	"github.com/sglavoie/deps-merger/pkg/github"
)

const login = "dmc[bot]"

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// pr is a clean, green Dependabot PR updating lib by a patch, as
// deps-merger's own fixtures build it.
func pr(n int) github.PullRequest {
	msg := "bump lib\n\n---\nupdated-dependencies:\n- dependency-name: lib\n  dependency-type: direct:production\n  update-type: version-update:semver-patch\n...\n\nSigned-off-by: dependabot[bot] <support@github.com>"
	return github.PullRequest{
		Number: n, Title: fmt.Sprintf("bump lib %d", n), URL: fmt.Sprintf("https://github.com/o/r/pull/%d", n),
		Author: "dependabot", HeadOID: fmt.Sprintf("sha%d", n), Mergeable: "MERGEABLE", MergeState: "CLEAN", CommitCount: 1,
		Commits: []github.Commit{{
			OID: fmt.Sprintf("sha%d", n), Message: msg, CommittedDate: t0,
			Authors:        []github.Author{{Login: "dependabot[bot]", Name: "dependabot[bot]"}},
			SignedByGitHub: true, CommittedByGitHub: true,
		}},
		Checks:    []github.Check{{Name: "build", State: github.CheckPassed}},
		ChecksOID: fmt.Sprintf("sha%d", n),
	}
}

// engine is GitHub as deps-merger's engine reads and writes it, for o/r.
type engine struct {
	mu       sync.Mutex
	prs      []github.PullRequest
	files    map[string]string
	merged   []int
	comments []int
	// gone are PRs someone else merged (true) or closed (false).
	gone map[int]bool
	// onMerge runs after a merge goes through.
	onMerge func(n int)
}

func (e *engine) Viewer(context.Context) (string, error) { return login, nil }

func (e *engine) FetchRepo(context.Context, string, string) (*github.Repo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return &github.Repo{FullName: "o/r", Permission: "WRITE", SquashAllowed: true, PullRequests: slices.Clone(e.prs),
		OpenPRCount: len(e.prs), SearchCount: len(e.prs), FetchedAt: t0.Add(time.Hour)}, nil
}

func (e *engine) Merge(_ context.Context, _, _ string, n int, _, _ string) error {
	e.mu.Lock()
	e.merged = append(e.merged, n)
	e.prs = slices.DeleteFunc(e.prs, func(p github.PullRequest) bool { return p.Number == n })
	hook := e.onMerge
	e.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return nil
}

func (e *engine) Comment(_ context.Context, _, _ string, n int, body string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.comments = append(e.comments, n)
	for i := range e.prs {
		if e.prs[i].Number == n {
			e.prs[i].Comments = append(e.prs[i].Comments, github.Comment{Author: login, Body: body})
			e.prs[i].CommentCount++
		}
	}
	return nil
}

func (e *engine) PullRequestDetails(context.Context, string, string, int) (*github.PullRequestDetails, error) {
	return &github.PullRequestDetails{}, nil
}

func (e *engine) PullRequestState(_ context.Context, _, _ string, n int) (github.PRState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if slices.Contains(e.merged, n) {
		return github.PRMerged, nil
	}
	if merged, ok := e.gone[n]; ok {
		if merged {
			return github.PRMerged, nil
		}
		return github.PRClosed, nil
	}
	return github.PROpen, nil
}

func (e *engine) FileText(_ context.Context, owner, name, path string) ([]byte, error) {
	if s, ok := e.files[owner+"/"+name+":"+path]; ok {
		return []byte(s), nil
	}
	return nil, nil
}

type platform struct {
	// installations, when set, replace the one installation, 1 on "o".
	installations []app.Installation
	engine        *engine
	issues        *ghfake.Issues
	rest          *gh.Client
	restErr       error
}

func (p *platform) Login() string { return login }
func (p *platform) Installations(context.Context) ([]app.Installation, error) {
	if p.installations != nil {
		return p.installations, nil
	}
	return []app.Installation{{ID: 1, Account: "o"}}, nil
}
func (p *platform) Repos(context.Context, int64) ([]string, error) { return []string{"o/r"}, nil }
func (p *platform) Engine(context.Context, int64) (github.Client, error) {
	return p.engine, nil
}
func (p *platform) REST(int64) (*gh.Client, error) { return p.rest, p.restErr }
func (p *platform) Token(context.Context, int64) (string, error) {
	return "ghs_test", nil
}

func setup(t *testing.T, allowMerge bool, repoFile string, prs ...github.PullRequest) (*Service, *platform) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "dmc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	issues := &ghfake.Issues{Repo: "o/r", Login: login}
	e := &engine{prs: prs, files: map[string]string{}}
	if repoFile != "" {
		e.files["o/r:.github/deps-merger.yml"] = repoFile
	}
	p := &platform{engine: e, issues: issues, rest: issues.Client(t)}
	s := &Service{
		Platform: p, Store: st, AllowMerge: allowMerge,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	return s, p
}

// writes lists the run's recorded writes as "kind #number".
func writes(t *testing.T, s *Service) []string {
	t.Helper()
	ws, err := s.Store.Writes(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, w := range ws {
		out = append(out, fmt.Sprintf("%s #%d", w.Kind, w.Number))
	}
	return out
}

func TestReportModeWritesOnlyTheIssue(t *testing.T) {
	// Merging is allowed on the deployment, but the repository asks for
	// nothing: the App's base policy is report mode.
	s, p := setup(t, true, "", pr(1), pr(2))
	s.RunRepo(context.Background(), 1, "o/r", "test")
	if len(p.engine.merged) != 0 || len(p.engine.comments) != 0 {
		t.Fatalf("merged %v, commented %v", p.engine.merged, p.engine.comments)
	}
	if got := writes(t, s); !slices.Equal(got, []string{"issue-created #1"}) {
		t.Fatalf("writes = %v", got)
	}
	if body := p.issues.Get(1).Body; !strings.Contains(body, "report mode") || !strings.Contains(body, "| would merge |") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestOnceReportsInaccessibleRepository(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	err := s.Once(context.Background(), []string{"other/repo"})
	if err == nil || !strings.Contains(err.Error(), "other/repo is not accessible to this App") {
		t.Fatalf("Once error = %v", err)
	}
	if len(p.engine.merged) != 0 || len(writes(t, s)) != 0 {
		t.Fatalf("Once changed a repository that was not requested")
	}
}

func TestOnceRunsRequestedRepository(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	if err := s.Once(context.Background(), []string{"o/r"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.engine.merged, []int{1}) {
		t.Fatalf("merged = %v", p.engine.merged)
	}
}

func TestOnceReportsRepositoryFailure(t *testing.T) {
	s, p := setup(t, true, "mode: yolo\n", pr(1))
	err := s.Once(context.Background(), []string{"o/r"})
	if err == nil || !strings.Contains(err.Error(), "o/r: ") || !strings.Contains(err.Error(), "invalid mode") {
		t.Fatalf("Once error = %v", err)
	}
	if len(p.engine.merged) != 0 {
		t.Fatalf("merged = %v", p.engine.merged)
	}
}

func TestOnceReportsIssueFailure(t *testing.T) {
	s, p := setup(t, false, "", pr(1))
	p.restErr = fmt.Errorf("GitHub issues unavailable")
	err := s.Once(context.Background(), []string{"o/r"})
	if err == nil || !strings.Contains(err.Error(), "o/r: issue: GitHub issues unavailable") {
		t.Fatalf("Once error = %v", err)
	}
}

func TestMergeModeMerges(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1), pr(2))
	s.RunRepo(context.Background(), 1, "o/r", "test")
	if !slices.Equal(p.engine.merged, []int{1, 2}) {
		t.Fatalf("merged %v", p.engine.merged)
	}
	ws, _ := s.Store.Writes(context.Background(), time.Time{})
	if len(ws) != 2 || ws[0].Kind != "merge" || ws[0].Detail != "squash at sha1" || ws[1].Number != 2 {
		t.Fatalf("writes = %+v", ws)
	}
	// Nothing left open: no issue is opened just to say so.
	if p.issues.Len() != 0 {
		t.Fatalf("%d issues", p.issues.Len())
	}
}

func TestMergeModeNeedsTheDeploymentSwitch(t *testing.T) {
	s, p := setup(t, false, "mode: merge\n", pr(1))
	s.RunRepo(context.Background(), 1, "o/r", "test")
	if len(p.engine.merged) != 0 {
		t.Fatalf("merged %v", p.engine.merged)
	}
	if body := p.issues.Get(1).Body; !strings.Contains(body, "paused or not enabled") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestPausedInstallationOnlyReports(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	if err := s.Store.Pause(context.Background(), 1, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.RunRepo(context.Background(), 1, "o/r", "test")
	if len(p.engine.merged) != 0 {
		t.Fatalf("merged %v", p.engine.merged)
	}
	if got := writes(t, s); !slices.Equal(got, []string{"issue-created #1"}) {
		t.Fatalf("writes = %v", got)
	}
}

func TestPauseStopsARunPartWay(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1), pr(2), pr(3))
	// Someone pulls the kill switch as the first merge lands.
	p.engine.onMerge = func(int) {
		if err := s.Store.Pause(context.Background(), 1, "stop", time.Now()); err != nil {
			t.Error(err)
		}
	}
	s.RunRepo(context.Background(), 1, "o/r", "test")
	if !slices.Equal(p.engine.merged, []int{1}) {
		t.Fatalf("merged %v", p.engine.merged)
	}
	got := writes(t, s)
	if len(got) < 2 || got[0] != "merge #1" || got[1] != "merge-refused #2" || slices.Contains(got, "merge #2") || slices.Contains(got, "merge #3") {
		t.Fatalf("writes = %v", got)
	}
}

func TestTheAppNeverPostsDependabotCommands(t *testing.T) {
	// A repository that turns Dependabot commands back on: the PR behind
	// its base would get @dependabot rebase from the CLI, but not from
	// the App.
	behind := pr(1)
	behind.MergeState = "BEHIND"
	s, p := setup(t, true, "mode: merge\ndependabot_commands: true\n", behind)
	s.RunRepo(context.Background(), 1, "o/r", "test")
	if len(p.engine.comments) != 0 || len(p.engine.merged) != 0 {
		t.Fatalf("commented %v, merged %v", p.engine.comments, p.engine.merged)
	}
	if body := p.issues.Get(1).Body; !strings.Contains(body, "dependabot\\_commands: false") {
		t.Fatalf("body does not say why:\n%s", body)
	}
}

func TestRunsOnlyAllowedRepositories(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	s.Allowed = []string{"o/other"}
	s.Run(context.Background(), jobs.Job{Installation: 1, Repo: "o/r"})
	s.Run(context.Background(), jobs.Job{Installation: 1})
	if len(p.engine.merged) != 0 || p.issues.Len() != 0 || len(writes(t, s)) != 0 {
		t.Fatalf("merged %v, %d issues", p.engine.merged, p.issues.Len())
	}
}

// leave takes PR n out of the open ones, as merged (by someone else) or
// closed.
func (e *engine) leave(n int, merged bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.prs = slices.DeleteFunc(e.prs, func(p github.PullRequest) bool { return p.Number == n })
	if e.gone == nil {
		e.gone = map[int]bool{}
	}
	e.gone[n] = merged
}

func TestRunsTrackPRsForTheMetrics(t *testing.T) {
	failing := pr(3)
	failing.Checks[0].State = github.CheckFailed
	s, p := setup(t, true, "mode: merge\n", pr(1), pr(2), failing)
	p.engine.prs[1].Checks[0].State = github.CheckPending // #2 waits for CI
	ctx := context.Background()
	if err := s.Store.InstallationAdded(ctx, 1, "o", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.RunRepo(ctx, 1, "o/r", "test")
	// #1 merged by the App; then a person merges #2 and closes #3.
	p.engine.leave(2, true)
	p.engine.leave(3, false)
	s.RunRepo(ctx, 1, "o/r", "test")

	ms, err := s.Store.Metrics(ctx, time.Now())
	if err != nil || len(ms) != 1 {
		t.Fatalf("metrics = %+v, %v", ms, err)
	}
	m := ms[0]
	if m.PRs != 3 || m.MergedByApp != 1 || m.MergedOther != 1 || m.ClosedUnmerged != 1 || m.Open != 0 || m.AppShare != 0.5 {
		t.Fatalf("metrics = %+v", m)
	}
	if m.Repos != 1 || m.ReposMerging != 1 || m.Merges != 1 {
		t.Fatalf("repos %d, merging %d, merges %d", m.Repos, m.ReposMerging, m.Merges)
	}
}

func TestInterruptedRunsStillRecordTheirMerges(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1), pr(2))
	ctx := context.Background()
	s.Store.InstallationAdded(ctx, 1, "o", time.Now())
	// The server stops as the first merge lands.
	run, stop := context.WithCancel(ctx)
	p.engine.onMerge = func(int) { stop() }
	s.RunRepo(run, 1, "o/r", "test")
	p.engine.onMerge = nil
	if !slices.Equal(p.engine.merged, []int{1}) {
		t.Fatalf("merged %v", p.engine.merged)
	}
	// A merge recorded only as a write, by a run that kept no PRs: #3 is
	// seen open, then the App's write says it merged it.
	p.engine.prs = append(p.engine.prs, pr(3))
	s.RunRepo(ctx, 1, "o/r", "test")
	at := time.Now().Add(-time.Minute).Round(time.Second)
	s.Store.RecordWrite(ctx, store.Write{RunID: 1, Repo: "o/r", Kind: "merge", Number: 3, At: at})
	p.engine.leave(3, true)
	s.RunRepo(ctx, 1, "o/r", "test")

	ms, _ := s.Store.Metrics(ctx, time.Now())
	if m := ms[0]; m.MergedByApp != 3 || m.MergedOther != 0 {
		t.Fatalf("by the App %d, by others %d; want 3 and 0", m.MergedByApp, m.MergedOther)
	}
}

func TestSweepForgetsUninstalledButNotSuspended(t *testing.T) {
	s, p := setup(t, false, "", pr(1))
	ctx := context.Background()
	p.installations = []app.Installation{{ID: 1, Account: "o"}, {ID: 2, Account: "p"}}
	if _, err := s.SyncInstallations(ctx); err != nil {
		t.Fatal(err)
	}
	s.RunRepo(ctx, 1, "o/r", "test")
	s.Store.SeenPR(ctx, 2, "p/r", 1, "x", time.Now(), time.Now())

	// 1 is suspended, 2 uninstalled while the server was down.
	p.installations = []app.Installation{{ID: 1, Account: "o", Suspended: true}}
	active, err := s.SyncInstallations(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("active = %v, %v", active, err)
	}
	ms, _ := s.Store.Metrics(ctx, time.Now())
	if len(ms) != 2 || ms[0].RemovedAt == nil || ms[1].RemovedAt == nil {
		t.Fatalf("metrics = %+v", ms)
	}
	if ms[0].PRs != 1 || ms[1].PRs != 0 {
		t.Fatalf("suspended kept %d PRs, uninstalled kept %d", ms[0].PRs, ms[1].PRs)
	}
}
