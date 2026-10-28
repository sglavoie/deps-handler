package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "dmc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func TestInstallationsComeAndGo(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.InstallationAdded(ctx, 1, "acme", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallationRemoved(ctx, 1, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Added again: active, still counted from the first install.
	if err := s.InstallationAdded(ctx, 1, "acme-renamed", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Installations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Account != "acme-renamed" || !got[0].AddedAt.Equal(t0) || got[0].RemovedAt != nil {
		t.Fatalf("installations = %+v", got)
	}
}

func TestStaleSweepCannotReactivateRemovedInstallation(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.InstallationRemoved(ctx, 7, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallationAdded(ctx, 7, "stale", t0); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.InstallationInactive(ctx, 7); err != nil || !inactive {
		t.Fatalf("after stale sweep: inactive = %t, %v", inactive, err)
	}
	if err := s.InstallationAdded(ctx, 7, "current", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.InstallationInactive(ctx, 7); err != nil || inactive {
		t.Fatalf("after fresh observation: inactive = %t, %v", inactive, err)
	}
}

func TestRunsIssuesAndWrites(t *testing.T) {
	s, ctx := open(t), context.Background()
	id, err := s.StartRun(ctx, 1, "acme/app", "schedule", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, id, RunOutcome{PRs: 3, WouldMerge: 2, Findings: 1}, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.IssueNumber(ctx, "acme/app"); err != nil || n != 0 {
		t.Fatalf("issue = %d, %v", n, err)
	}
	if err := s.SetIssueNumber(ctx, "acme/app", 7); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIssueNumber(ctx, "acme/app", 9); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.IssueNumber(ctx, "acme/app"); n != 9 {
		t.Fatalf("issue = %d", n)
	}
	if err := s.RecordWrite(ctx, Write{RunID: id, Repo: "acme/app", Kind: "issue-created", Number: 9, At: t0}); err != nil {
		t.Fatal(err)
	}
	ws, err := s.Writes(ctx, t0.Add(-time.Minute))
	if err != nil || len(ws) != 1 || ws[0].Kind != "issue-created" || ws[0].Number != 9 {
		t.Fatalf("writes = %+v, %v", ws, err)
	}
	// A write must belong to a run.
	if err := s.RecordWrite(ctx, Write{RunID: 999, Repo: "acme/app", Kind: "x", At: t0}); err == nil {
		t.Fatal("write for an unknown run was accepted")
	}
}

func TestPauseAndResume(t *testing.T) {
	s, ctx := open(t), context.Background()
	if p, err := s.Paused(ctx, 1); err != nil || p {
		t.Fatalf("paused = %v, %v", p, err)
	}
	if err := s.Pause(ctx, 1, "wrong merge", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx, 1, "again", t0); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Paused(ctx, 1); !p {
		t.Fatal("not paused")
	}
	if p, _ := s.Paused(ctx, 2); p {
		t.Fatal("another installation paused")
	}
	if err := s.Resume(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Paused(ctx, 1); p {
		t.Fatal("still paused")
	}
}

func TestTwoProcessesShareTheDatabase(t *testing.T) {
	// The admin command pauses through its own connection while the
	// server runs.
	path := filepath.Join(t.TempDir(), "dmc.db")
	ctx := context.Background()
	a, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Pause(ctx, 7, "", t0); err != nil {
		t.Fatal(err)
	}
	if p, err := a.Paused(ctx, 7); err != nil || !p {
		t.Fatalf("paused = %v, %v", p, err)
	}
}

func TestOpenEscapesSQLiteURIPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state?backup#1.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at %q: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "state")); !os.IsNotExist(err) {
		t.Fatalf("URI path was truncated: %v", err)
	}
}

func TestOpenRelativeSQLitePath(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := Open(context.Background(), "state.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("state.db"); err != nil {
		t.Fatalf("relative database was not created: %v", err)
	}
}

func TestPRLifecycleAndMetrics(t *testing.T) {
	s, ctx := open(t), context.Background()
	added := t0
	if err := s.InstallationAdded(ctx, 1, "acme", added); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	// Two PRs waiting for 10 and 20 days when the App arrives.
	s.SeenPR(ctx, 1, "acme/app", 1, "old a", added.Add(-10*day), added)
	s.SeenPR(ctx, 1, "acme/app", 2, "old b", added.Add(-20*day), added)
	// Two opened since.
	s.SeenPR(ctx, 1, "acme/app", 3, "new a", added.Add(day), added.Add(day))
	s.SeenPR(ctx, 1, "acme/app", 4, "new b", added.Add(2*day), added.Add(2*day))
	s.ClosePR(ctx, "acme/app", 1, PRMergedByApp, added.Add(time.Hour))
	s.ClosePR(ctx, "acme/app", 3, PRMergedByApp, added.Add(day+2*time.Hour))
	s.ClosePR(ctx, "acme/app", 2, PRClosed, added.Add(3*day))
	if open, _ := s.OpenPRs(ctx, "acme/app"); len(open) != 1 || open[0] != 4 {
		t.Fatalf("open = %v", open)
	}
	// Seen open again (reopened): open once more.
	s.SeenPR(ctx, 1, "acme/app", 2, "old b", added.Add(-20*day), added.Add(4*day))
	s.ClosePR(ctx, "acme/app", 2, PRClosed, added.Add(5*day))
	// A stale read of a merged PR leaves it merged.
	s.SeenPR(ctx, 1, "acme/app", 1, "old a", added.Add(-10*day), added.Add(4*day))
	s.SeenRepo(ctx, 1, "acme/app", "merge", true, added)
	s.SeenRepo(ctx, 1, "acme/lib", "report", false, added)

	now := added.Add(30 * day)
	ms, err := s.Metrics(ctx, now)
	if err != nil || len(ms) != 1 {
		t.Fatalf("metrics = %+v, %v", ms, err)
	}
	m := ms[0]
	if m.Repos != 2 || m.ReposMerging != 1 || m.PRs != 4 || m.Open != 1 || m.MergedByApp != 2 || m.MergedOther != 0 || m.ClosedUnmerged != 1 {
		t.Fatalf("counts = %+v", m)
	}
	if m.AppShare != 1 {
		t.Errorf("app share = %v", m.AppShare)
	}
	if m.OpenAgeAtStart != 15*day {
		t.Errorf("open age at start = %v", m.OpenAgeAtStart)
	}
	if m.OpenAgeNow != 28*day {
		t.Errorf("open age now = %v", m.OpenAgeNow)
	}
	if m.MergeTimeApp != (10*day+time.Hour+2*time.Hour)/2 {
		t.Errorf("merge time = %v", m.MergeTimeApp)
	}
	if m.Days != 30 || m.Retained == nil || !*m.Retained {
		t.Errorf("days %d, retained %v", m.Days, m.Retained)
	}
}

func TestRetention(t *testing.T) {
	s, ctx := open(t), context.Background()
	day := 24 * time.Hour
	s.InstallationAdded(ctx, 1, "young", t0)
	s.InstallationAdded(ctx, 2, "left", t0)
	s.InstallationRemoved(ctx, 2, t0.Add(10*day))
	ms, _ := s.Metrics(ctx, t0.Add(20*day))
	if ms[0].Retained != nil || ms[1].Retained != nil {
		t.Fatal("retention decided before day 28")
	}
	ms, _ = s.Metrics(ctx, t0.Add(29*day))
	if ms[0].Retained == nil || !*ms[0].Retained || ms[1].Retained == nil || *ms[1].Retained || ms[1].Days != 10 {
		t.Fatalf("metrics = %+v", ms)
	}
}

func TestStaleRepositorySweepCannotUndoWebhook(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.SeenRepo(ctx, 1, "Acme/App", "merge", true, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RepoAccess(ctx, 1, "acme/app", false, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRepos(ctx, 1, []string{"ACME/APP"}, t0); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.RepoInactive(ctx, 1, "Acme/App"); err != nil || !inactive {
		t.Fatalf("stale sweep reactivated repository: inactive = %t, %v", inactive, err)
	}
	if err := s.RepoAccess(ctx, 1, "ACME/APP", true, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRepos(ctx, 1, nil, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.RepoInactive(ctx, 1, "acme/app"); err != nil || inactive {
		t.Fatalf("stale sweep removed access after add: inactive = %t, %v", inactive, err)
	}
}

func TestOpenAddsRepositoryAccessToExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dmc.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SeenRepo(ctx, 1, "acme/app", "merge", true, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE repo_access`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReconcileRepos(ctx, 1, nil, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.RepoInactive(ctx, 1, "acme/app"); err != nil || !inactive {
		t.Fatalf("existing repository inactive = %t, %v", inactive, err)
	}
}

func TestForgetInstallation(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.InstallationAdded(ctx, 1, "acme", t0)
	s.InstallationAdded(ctx, 2, "other", t0)
	for _, in := range []struct {
		id   int64
		repo string
	}{{1, "acme/app"}, {2, "other/app"}} {
		run, _ := s.StartRun(ctx, in.id, in.repo, "test", t0)
		s.RecordWrite(ctx, Write{RunID: run, Repo: in.repo, Kind: "merge", Number: 1, At: t0})
		s.SetIssueNumber(ctx, in.repo, 5)
		s.SeenPR(ctx, in.id, in.repo, 1, "x", t0, t0)
		s.SeenRepo(ctx, in.id, in.repo, "merge", true, t0)
	}
	s.Pause(ctx, 1, "", t0)
	if err := s.ForgetInstallation(ctx, 1); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.Writes(ctx, time.Time{})
	if len(ws) != 1 || ws[0].Repo != "other/app" {
		t.Fatalf("writes = %+v", ws)
	}
	if n, _ := s.IssueNumber(ctx, "acme/app"); n != 0 {
		t.Fatal("issue kept")
	}
	if n, _ := s.IssueNumber(ctx, "other/app"); n != 5 {
		t.Fatal("another installation's issue forgotten")
	}
	if p, _ := s.Paused(ctx, 1); p {
		t.Fatal("pause kept")
	}
	ms, _ := s.Metrics(ctx, t0)
	if len(ms) != 2 || ms[0].PRs != 0 || ms[0].Repos != 0 || ms[1].PRs != 1 {
		t.Fatalf("metrics = %+v", ms)
	}
}

func TestReviews(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.InstallationAdded(ctx, 1, "acme", t0)
	for i, v := range []string{"approve", "reject", "failed: timeout"} {
		if err := s.RecordReview(ctx, Review{Installation: 1, Repo: "acme/app", Number: i + 1, Model: "qwen", Verdict: v,
			Requests: 3, PromptTokens: 1000, CompletionTokens: 50, Took: time.Minute, At: t0.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.ReviewsSince(ctx, 1, t0.Add(30*time.Minute)); n != 2 {
		t.Errorf("since = %d", n)
	}
	ms, _ := s.Metrics(ctx, t0.Add(3*time.Hour))
	if m := ms[0]; m.Reviews != 3 || m.ReviewsApproved != 1 || m.PromptTokens != 3000 || m.CompletionTokens != 150 {
		t.Fatalf("metrics = %+v", m)
	}
	s.ForgetInstallation(ctx, 1)
	if n, _ := s.ReviewsSince(ctx, 1, time.Time{}); n != 0 {
		t.Fatal("reviews kept after forgetting")
	}
}
