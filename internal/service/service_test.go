package service

import (
	"context"
	"database/sql"
	"errors"
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
	"github.com/sglavoie/deps-merger/pkg/runner"
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
	onMerge  func(n int)
	onState  func()
	onFile   func()
	mergeErr error
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
	if e.mergeErr != nil {
		err := e.mergeErr
		e.mu.Unlock()
		return err
	}
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
	if e.onState != nil {
		e.onState()
	}
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
	if e.onFile != nil {
		e.onFile()
	}
	if s, ok := e.files[owner+"/"+name+":"+path]; ok {
		return []byte(s), nil
	}
	return nil, nil
}

type platform struct {
	// installations, when set, replace the one installation, 1 on "o".
	installations []app.Installation
	repos         []string
	onList        func()
	onRepos       func()
	dbPath        string
	engine        *engine
	issues        *ghfake.Issues
	rest          *gh.Client
	restErr       error
	tokenErr      error
}

func (p *platform) Login() string { return login }
func (p *platform) Installations(context.Context) ([]app.Installation, error) {
	if p.onList != nil {
		p.onList()
	}
	if p.installations != nil {
		return p.installations, nil
	}
	return []app.Installation{{ID: 1, Account: "o"}}, nil
}
func (p *platform) Repos(context.Context, int64) ([]string, error) {
	if p.onRepos != nil {
		p.onRepos()
	}
	if p.repos != nil {
		return slices.Clone(p.repos), nil
	}
	return []string{"o/r"}, nil
}
func (p *platform) Engine(context.Context, int64) (github.Client, error) {
	return p.engine, nil
}
func (p *platform) REST(int64) (*gh.Client, error) { return p.rest, p.restErr }
func (p *platform) Token(context.Context, int64) (string, error) {
	if p.tokenErr != nil {
		return "", p.tokenErr
	}
	return "ghs_test", nil
}

func setup(t *testing.T, allowMerge bool, repoFile string, prs ...github.PullRequest) (*Service, *platform) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dmc.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	issues := &ghfake.Issues{Repo: "o/r", Login: login}
	e := &engine{prs: prs, files: map[string]string{}}
	if repoFile != "" {
		e.files["o/r:.github/deps-merger.yml"] = repoFile
	}
	p := &platform{dbPath: dbPath, engine: e, issues: issues, rest: issues.Client(t)}
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

func rejectInsert(t *testing.T, dbPath, table string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	query := fmt.Sprintf("CREATE TRIGGER fail_insert BEFORE INSERT ON %s BEGIN SELECT RAISE(FAIL, 'database write unavailable'); END", table)
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestOnceReportsTrackingFailure(t *testing.T) {
	s, p := setup(t, false, "", pr(1))
	db := rejectInsert(t, p.dbPath, "repos")
	err := s.Once(context.Background(), []string{"o/r"})
	if err == nil || !strings.Contains(err.Error(), "track repository:") || !strings.Contains(err.Error(), "database write unavailable") {
		t.Fatalf("Once error = %v", err)
	}
	var recorded string
	if err := db.QueryRow("SELECT error FROM runs ORDER BY id DESC LIMIT 1").Scan(&recorded); err != nil || !strings.Contains(recorded, "track repository") {
		t.Fatalf("recorded error = %q, %v", recorded, err)
	}
}

func TestOnceReportsAuditFailureWithoutRetryingMerge(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	db := rejectInsert(t, p.dbPath, "writes")
	err := s.Once(context.Background(), []string{"o/r"})
	if err == nil || !strings.Contains(err.Error(), "record merge #1:") || !strings.Contains(err.Error(), "database write unavailable") {
		t.Fatalf("Once error = %v", err)
	}
	if !slices.Equal(p.engine.merged, []int{1}) {
		t.Fatalf("merges = %v", p.engine.merged)
	}
	var recorded string
	if err := db.QueryRow("SELECT error FROM runs ORDER BY id DESC LIMIT 1").Scan(&recorded); err != nil || !strings.Contains(recorded, "record merge #1") {
		t.Fatalf("recorded error = %q, %v", recorded, err)
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

type stateClient struct {
	github.Client
	state github.PRState
}

func (c stateClient) PullRequestState(context.Context, string, string, int) (github.PRState, error) {
	return c.state, nil
}

func TestTrackingUnknownPRStateKeepsPROpen(t *testing.T) {
	for _, state := range []github.PRState{"", "UNKNOWN"} {
		t.Run(string(state), func(t *testing.T) {
			s, p := setup(t, false, "")
			ctx := context.Background()
			if err := s.Store.SeenPR(ctx, 1, "o/r", 1, "bump lib", t0, t0); err != nil {
				t.Fatal(err)
			}
			client := stateClient{Client: p.engine, state: state}
			err := s.track(ctx, s.Log, client, 1, "o/r", runner.RepoResult{}, nil, nil, false, true)
			if err == nil || !strings.Contains(err.Error(), "state") {
				t.Fatalf("unknown PR state returned %v", err)
			}
			open, err := s.Store.OpenPRs(ctx, "o/r")
			if err != nil || !slices.Equal(open, []int{1}) {
				t.Fatalf("unknown PR state changed tracked PRs: %v, %v", open, err)
			}
		})
	}
}

func TestFailedMergeAttemptIsNotCreditedToApp(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	ctx := context.Background()
	if err := s.Store.InstallationAdded(ctx, 1, "o", time.Now()); err != nil {
		t.Fatal(err)
	}
	p.engine.mergeErr = errors.New("merge refused")
	if err := s.RunRepo(ctx, 1, "o/r", "test"); err == nil {
		t.Fatal("failed merge returned no error")
	}
	if !slices.Contains(writes(t, s), "merge-attempted #1") {
		t.Fatalf("missing attempt record: %v", writes(t, s))
	}
	p.engine.leave(1, true) // A person merges it after the refusal.
	if err := s.RunRepo(ctx, 1, "o/r", "test"); err != nil {
		t.Fatal(err)
	}
	ms, err := s.Store.Metrics(ctx, time.Now())
	if err != nil || len(ms) != 1 {
		t.Fatalf("metrics = %+v, %v", ms, err)
	}
	if ms[0].MergedByApp != 0 || ms[0].MergedOther != 1 {
		t.Fatalf("merge attribution = %+v", ms[0])
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

func TestCancellationDuringTrackingRecordsFailedRun(t *testing.T) {
	s, p := setup(t, false, "", pr(1))
	ctx := context.Background()
	if err := s.Store.InstallationAdded(ctx, 1, "o", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RunRepo(ctx, 1, "o/r", "test"); err != nil {
		t.Fatal(err)
	}
	p.engine.leave(1, false)
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	p.engine.onState = cancel
	if err := s.RunRepo(run, 1, "o/r", "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run error = %v", err)
	}
	db, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var recorded string
	if err := db.QueryRow("SELECT error FROM runs ORDER BY id DESC LIMIT 1").Scan(&recorded); err != nil || !strings.Contains(recorded, "context canceled") {
		t.Fatalf("recorded error = %q, %v", recorded, err)
	}
}

func TestRepositoryRemovalUpdatesCurrentMetricsAndStopsRuns(t *testing.T) {
	s, _ := setup(t, false, "", pr(1))
	ctx := context.Background()
	if err := s.Store.InstallationAdded(ctx, 1, "o", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RunRepo(ctx, 1, "o/r", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.RepositoryRemoved(ctx, 1, "o/r"); err != nil {
		t.Fatal(err)
	}
	ms, err := s.Store.Metrics(ctx, time.Now())
	if err != nil || len(ms) != 1 || ms[0].Repos != 0 || ms[0].Open != 0 || ms[0].PRs != 1 {
		t.Fatalf("removed repository metrics = %+v, %v", ms, err)
	}
	if err := s.RunRepo(ctx, 1, "o/r", "stale webhook"); err != nil {
		t.Fatal(err)
	}
	if err := s.RepositoryAdded(ctx, 1, "o/r"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunRepo(ctx, 1, "o/r", "added"); err != nil {
		t.Fatal(err)
	}
	ms, err = s.Store.Metrics(ctx, time.Now())
	if err != nil || len(ms) != 1 || ms[0].Repos != 1 || ms[0].Open != 1 {
		t.Fatalf("readded repository metrics = %+v, %v", ms, err)
	}
}

func TestScheduledRunReconcilesMissedRepositoryRemoval(t *testing.T) {
	s, p := setup(t, false, "", pr(1))
	ctx := context.Background()
	if err := s.Store.InstallationAdded(ctx, 1, "o", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RunRepo(ctx, 1, "o/r", "test"); err != nil {
		t.Fatal(err)
	}
	p.repos = []string{}
	if err := s.Run(ctx, jobs.Job{Installation: 1, Trigger: "schedule"}); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.RepoInactive(ctx, 1, "o/r"); err != nil || !inactive {
		t.Fatalf("repository inactive = %t, %v", inactive, err)
	}
	ms, err := s.Store.Metrics(ctx, time.Now())
	if err != nil || ms[0].Repos != 0 || ms[0].Open != 0 {
		t.Fatalf("reconciled metrics = %+v, %v", ms, err)
	}
	p.repos = []string{"o/r"}
	if err := s.Run(ctx, jobs.Job{Installation: 1, Trigger: "schedule"}); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.RepoInactive(ctx, 1, "o/r"); err != nil || inactive {
		t.Fatalf("repository inactive after access restored = %t, %v", inactive, err)
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

func TestSweepRecordsFirstSeenSuspendedInstallation(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	ctx := context.Background()
	p.installations = []app.Installation{{ID: 1, Account: "o", Suspended: true}}
	active, err := s.SyncInstallations(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("active = %v, %v", active, err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || !inactive {
		t.Fatalf("first-seen suspended installation inactive = %t, %v", inactive, err)
	}
	ins, err := s.Store.Installations(ctx)
	if err != nil || len(ins) != 1 || ins[0].Account != "o" || ins[0].RemovedAt == nil {
		t.Fatalf("recorded installations = %+v, %v", ins, err)
	}
	if err := s.Run(ctx, jobs.Job{Installation: 1, Repo: "o/r", Trigger: "stale webhook"}); err != nil {
		t.Fatal(err)
	}
	if len(p.engine.merged) != 0 || len(writes(t, s)) != 0 {
		t.Fatalf("suspended installation merged %v, wrote %v", p.engine.merged, writes(t, s))
	}
	p.installations[0].Suspended = false
	active, err = s.SyncInstallations(ctx)
	if err != nil || !slices.Equal(active, []int64{1}) {
		t.Fatalf("active after unsuspend = %v, %v", active, err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || inactive {
		t.Fatalf("unsuspended installation inactive = %t, %v", inactive, err)
	}
}

func TestQueuedJobSkipsSuspendedInstallation(t *testing.T) {
	s, p := setup(t, true, "mode: merge\n", pr(1))
	ctx := context.Background()
	if err := s.InstallationAdded(ctx, 1, "o"); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallationRemoved(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, jobs.Job{Installation: 1, Repo: "o/r", Trigger: "stale webhook"}); err != nil {
		t.Fatal(err)
	}
	if len(p.engine.merged) != 0 || len(writes(t, s)) != 0 {
		t.Fatalf("inactive installation merged %v, wrote %v", p.engine.merged, writes(t, s))
	}
	ms, err := s.Store.Metrics(ctx, time.Now())
	if err != nil || len(ms) != 1 || ms[0].Repos != 0 {
		t.Fatalf("metrics = %+v, %v", ms, err)
	}
}

func TestSweepSnapshotCannotUndoUninstallWebhook(t *testing.T) {
	s, p := setup(t, false, "", pr(1))
	ctx := context.Background()
	if err := s.InstallationAdded(ctx, 1, "o"); err != nil {
		t.Fatal(err)
	}
	p.onList = func() {
		if err := s.InstallationRemoved(ctx, 1, true); err != nil {
			t.Error(err)
		}
	}
	active, err := s.SyncInstallations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("stale sweep returned active installations: %v", active)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || !inactive {
		t.Fatalf("stale sweep reactivated installation: inactive = %t, %v", inactive, err)
	}
	if err := s.Run(ctx, jobs.Job{Installation: 1, Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	if len(writes(t, s)) != 0 {
		t.Fatalf("stale sweep caused writes: %v", writes(t, s))
	}
}

func TestSweepSnapshotCannotRemoveNewInstallationWebhook(t *testing.T) {
	s, p := setup(t, false, "")
	ctx := context.Background()
	// The list was fetched before this webhook arrived.
	p.installations = []app.Installation{}
	p.onList = func() {
		if err := s.InstallationAdded(ctx, 2, "new-owner"); err != nil {
			t.Error(err)
		}
	}
	if _, err := s.SyncInstallations(ctx); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 2); err != nil || inactive {
		t.Fatalf("new installation was removed by stale sweep: inactive = %t, %v", inactive, err)
	}
}

func TestSweepSnapshotCannotResuspendInstallationAfterUnsuspendWebhook(t *testing.T) {
	s, p := setup(t, false, "")
	ctx := context.Background()
	if err := s.InstallationAdded(ctx, 1, "o"); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallationRemoved(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	p.installations = []app.Installation{{ID: 1, Account: "o", Suspended: true}}
	p.onList = func() {
		if err := s.InstallationAdded(ctx, 1, "o"); err != nil {
			t.Error(err)
		}
	}
	if _, err := s.SyncInstallations(ctx); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || inactive {
		t.Fatalf("unsuspended installation was removed by stale sweep: inactive = %t, %v", inactive, err)
	}
}

func TestInstallationWebhookUsesCurrentStateAfterDelayedDelivery(t *testing.T) {
	s, p := setup(t, false, "")
	ctx := context.Background()
	p.installations = []app.Installation{{ID: 1, Account: "o"}}
	if err := s.SyncInstallationWebhook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	p.installations[0].Suspended = true
	if err := s.SyncInstallationWebhook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || !inactive {
		t.Fatalf("suspended installation inactive = %t, %v", inactive, err)
	}
	p.installations[0].Suspended = false
	if err := s.SyncInstallationWebhook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	// An old suspend or delete delivery is processed after the unsuspend.
	if err := s.SyncInstallationWebhook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || inactive {
		t.Fatalf("delayed delivery changed active installation: inactive = %t, %v", inactive, err)
	}
	p.installations = []app.Installation{}
	if err := s.SyncInstallationWebhook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	// Likewise an old created delivery must not restore an uninstalled App.
	if err := s.SyncInstallationWebhook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || !inactive {
		t.Fatalf("delayed delivery restored deleted installation: inactive = %t, %v", inactive, err)
	}
}

func TestInstallationWebhookSnapshotCannotUndoNewerChange(t *testing.T) {
	s, p := setup(t, false, "")
	ctx := context.Background()
	p.installations = []app.Installation{{ID: 1, Account: "o"}}
	p.onList = func() {
		p.onList = nil
		if err := s.InstallationRemoved(ctx, 1, false); err != nil {
			t.Error(err)
		}
	}
	if err := s.SyncInstallationWebhook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || !inactive {
		t.Fatalf("stale snapshot restored suspended installation: inactive = %t, %v", inactive, err)
	}
}

func TestRepositoryWebhookUsesCurrentAccessAfterDelayedDelivery(t *testing.T) {
	s, p := setup(t, false, "")
	ctx := context.Background()
	p.repos = []string{"o/r"}
	if err := s.SyncRepositoryWebhook(ctx, 1, "o/r"); err != nil {
		t.Fatal(err)
	}
	p.repos = []string{}
	if err := s.SyncRepositoryWebhook(ctx, 1, "o/r"); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.RepoInactive(ctx, 1, "o/r"); err != nil || !inactive {
		t.Fatalf("removed repository inactive = %t, %v", inactive, err)
	}
	p.repos = []string{"O/R"}
	if err := s.SyncRepositoryWebhook(ctx, 1, "o/r"); err != nil {
		t.Fatal(err)
	}
	// An old removal after the add still observes current access.
	if err := s.SyncRepositoryWebhook(ctx, 1, "o/r"); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.RepoInactive(ctx, 1, "o/r"); err != nil || inactive {
		t.Fatalf("delayed delivery removed current access: inactive = %t, %v", inactive, err)
	}
}

func TestRepositoryWebhookSnapshotCannotUndoNewerChange(t *testing.T) {
	s, p := setup(t, false, "")
	ctx := context.Background()
	p.repos = []string{"o/r"}
	p.onRepos = func() {
		p.onRepos = nil
		if err := s.RepositoryRemoved(ctx, 1, "o/r"); err != nil {
			t.Error(err)
		}
	}
	if err := s.SyncRepositoryWebhook(ctx, 1, "o/r"); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.RepoInactive(ctx, 1, "o/r"); err != nil || !inactive {
		t.Fatalf("stale snapshot restored repository access: inactive = %t, %v", inactive, err)
	}
}

func TestFailedSweepRetriesSoonerThanItsPeriod(t *testing.T) {
	period := 4 * time.Hour
	for _, c := range []struct {
		failures int
		want     time.Duration
	}{{0, period}, {1, time.Minute}, {2, 2 * time.Minute}, {3, 5 * time.Minute}, {4, 15 * time.Minute}, {9, 15 * time.Minute}} {
		if got := sweepDelay(period, c.failures); got != c.want {
			t.Errorf("after %d failures, delay = %s, want %s", c.failures, got, c.want)
		}
	}
}

func TestQueuedJobReturnsRepositoryFailureForRetry(t *testing.T) {
	s, p := setup(t, false, "", pr(1))
	p.restErr = errors.New("issues temporarily unavailable")
	err := s.Run(context.Background(), jobs.Job{Installation: 1, Repo: "o/r", Trigger: "webhook"})
	if err == nil || !strings.Contains(err.Error(), "issues temporarily unavailable") {
		t.Fatalf("queued run error = %v", err)
	}
}
