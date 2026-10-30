package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRunSnapshotRespectsCleanupAndSuspension(t *testing.T) {
	for _, state := range []string{"active", "suspended", "forgotten", "reactivated"} {
		t.Run(state, func(t *testing.T) {
			s, ctx := open(t), context.Background()
			if err := s.InstallationAdded(ctx, 1, "o", t0); err != nil {
				t.Fatal(err)
			}
			runID, err := s.StartRun(ctx, 1, "O/R", "test", t0)
			if err != nil {
				t.Fatal(err)
			}
			if state != "active" {
				if err := s.InstallationRemoved(ctx, 1, t0.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			forgotten := state == "forgotten" || state == "reactivated"
			if forgotten {
				if err := s.ForgetInstallation(ctx, 1); err != nil {
					t.Fatal(err)
				}
			}
			if state == "reactivated" {
				if err := s.InstallationAdded(ctx, 1, "o", t0.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
				currentID, err := s.StartRun(ctx, 1, "o/r", "new run", t0.Add(2*time.Minute))
				if err != nil || currentID == runID {
					t.Fatalf("new run = %d, old run = %d, err = %v", currentID, runID, err)
				}
				if err := s.SeenRepo(ctx, 1, "o/r", "report", false, t0); err != nil {
					t.Fatal(err)
				}
				if err := s.SeenPR(ctx, 1, "o/r", 1, "current title", t0, t0); err != nil {
					t.Fatal(err)
				}
				if err := s.SetRunIssueNumber(ctx, currentID, 9); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := RunSnapshot{Mode: "merge", Merging: true, Complete: true, At: t0.Add(3 * time.Minute),
				PRs: []PRObservation{
					{Number: 1, Title: "old title", OpenedAt: t0},
					{Number: 2, Title: "merged update", OpenedAt: t0},
				}, Closed: []PRClosure{{Number: 2, Outcome: PRMergedByApp, At: t0.Add(time.Minute)}}}
			if recorded, err := s.RecordRunSnapshot(ctx, runID, snapshot); err != nil || recorded == forgotten {
				t.Fatalf("recorded = %t, err = %v; forgotten = %t", recorded, err, forgotten)
			}
			if err := s.SetRunIssueNumber(ctx, runID, 7); err != nil {
				t.Fatal(err)
			}
			metrics, err := s.Metrics(ctx, t0.Add(time.Hour))
			if err != nil || len(metrics) != 1 {
				t.Fatalf("metrics = %+v, %v", metrics, err)
			}
			m := metrics[0]
			wantIssue := 7
			switch state {
			case "forgotten":
				wantIssue = 0
				if m.Repos != 0 || m.PRs != 0 {
					t.Fatalf("late snapshot restored forgotten records: %+v", m)
				}
			case "reactivated":
				wantIssue = 9
				if m.Repos != 1 || m.ReposMerging != 0 || m.PRs != 1 || m.MergedByApp != 0 {
					t.Fatalf("late snapshot changed current records: %+v", m)
				}
				var title string
				if err := s.db.QueryRowContext(ctx, `SELECT title FROM prs WHERE repo = 'o/r' AND number = 1`).Scan(&title); err != nil || title != "current title" {
					t.Fatalf("current title = %q, %v", title, err)
				}
			default:
				if m.Repos != 1 || m.ReposMerging != 1 || m.PRs != 2 || m.Open != 1 || m.MergedByApp != 1 || m.MergeTimeApp != time.Minute {
					t.Fatalf("run evidence lost: %+v", m)
				}
			}
			if n, err := s.IssueNumber(ctx, "o/r"); err != nil || n != wantIssue {
				t.Fatalf("issue = %d, %v; want %d", n, err, wantIssue)
			}
		})
	}
}

func TestPartialRunSnapshotPreservesPREvidenceWithoutChangingMode(t *testing.T) {
	s, ctx := open(t), context.Background()
	runID, err := s.StartRun(ctx, 1, "o/r", "interrupted", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SeenRepo(ctx, 1, "o/r", "report", false, t0); err != nil {
		t.Fatal(err)
	}
	snapshot := RunSnapshot{Mode: "merge", Merging: true, Complete: false, At: t0,
		PRs:    []PRObservation{{Number: 1, Title: "update", OpenedAt: t0}},
		Closed: []PRClosure{{Number: 1, Outcome: PRMergedByApp, At: t0.Add(time.Minute)}}}
	if recorded, err := s.RecordRunSnapshot(ctx, runID, snapshot); err != nil || !recorded {
		t.Fatalf("recorded = %t, %v", recorded, err)
	}
	var mode, outcome string
	if err := s.db.QueryRowContext(ctx, `SELECT mode FROM repos WHERE repo = 'o/r'`).Scan(&mode); err != nil || mode != "report" {
		t.Fatalf("mode = %q, %v", mode, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT outcome FROM prs WHERE repo = 'o/r' AND number = 1`).Scan(&outcome); err != nil || outcome != PRMergedByApp {
		t.Fatalf("outcome = %q, %v", outcome, err)
	}
}

func TestRunSnapshotRollsBackOnFailure(t *testing.T) {
	s, ctx := open(t), context.Background()
	runID, err := s.StartRun(ctx, 1, "o/r", "test", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_pr BEFORE INSERT ON prs BEGIN SELECT RAISE(ABORT, 'cannot store PR'); END`); err != nil {
		t.Fatal(err)
	}
	snapshot := RunSnapshot{Mode: "merge", Merging: true, Complete: true, At: t0,
		PRs: []PRObservation{{Number: 1, Title: "update", OpenedAt: t0}}}
	if recorded, err := s.RecordRunSnapshot(ctx, runID, snapshot); err == nil || recorded {
		t.Fatalf("failed snapshot recorded = %t, %v", recorded, err)
	}
	var repos int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repos`).Scan(&repos); err != nil || repos != 0 {
		t.Fatalf("partial snapshot committed: %d repositories, %v", repos, err)
	}
}

func TestStartRunCannotRestoreInactiveInstallationRecords(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.InstallationRemoved(ctx, 1, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetInstallation(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if id, err := s.StartRun(ctx, 1, "o/r", "late event", t0); !errors.Is(err, ErrInstallationInactive) || id != 0 {
		t.Fatalf("inactive run = %d, %v", id, err)
	}
	var runs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("late start restored %d runs, %v", runs, err)
	}
}

func TestConcurrentSnapshotAndCleanupLeaveNoRecords(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "shared.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for range 20 {
		runID, err := s.StartRun(ctx, 1, "o/r", "test", t0)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := RunSnapshot{Mode: "report", Complete: true, At: t0,
			PRs: []PRObservation{{Number: 1, Title: "update", OpenedAt: t0}}}
		start, done := make(chan struct{}), make(chan error, 2)
		go func() {
			<-start
			_, err := s.RecordRunSnapshot(ctx, runID, snapshot)
			done <- err
		}()
		go func() {
			<-start
			done <- other.ForgetInstallation(ctx, 1)
		}()
		close(start)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM repos) + (SELECT COUNT(*) FROM prs)`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("snapshot escaped cleanup: %d retained records, %v", count, err)
		}
	}
}

func TestLateSnapshotCannotDowngradeConfirmedMerge(t *testing.T) {
	for _, tc := range []struct {
		name, first, late, want string
	}{
		{"merged then closed", PRMerged, PRClosed, PRMerged},
		{"app merge then closed", PRMergedByApp, PRClosed, PRMergedByApp},
		{"app merge then unattributed merge", PRMergedByApp, PRMerged, PRMergedByApp},
		{"repeated merge", PRMerged, PRMerged, PRMerged},
		{"repeated app merge", PRMergedByApp, PRMergedByApp, PRMergedByApp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := open(t), context.Background()
			firstRun, err := s.StartRun(ctx, 1, "o/r", "first", t0)
			if err != nil {
				t.Fatal(err)
			}
			lateRun, err := s.StartRun(ctx, 1, "o/r", "overlapping", t0)
			if err != nil {
				t.Fatal(err)
			}
			for i, runID := range []int64{firstRun, lateRun} {
				outcome := tc.first
				if i == 1 {
					outcome = tc.late
				}
				at := t0.Add(time.Duration(i+1) * time.Minute)
				snapshot := RunSnapshot{At: at,
					PRs:    []PRObservation{{Number: 1, Title: "update", OpenedAt: t0}},
					Closed: []PRClosure{{Number: 1, Outcome: outcome, At: at}}}
				if recorded, err := s.RecordRunSnapshot(ctx, runID, snapshot); err != nil || !recorded {
					t.Fatalf("recorded = %t, %v", recorded, err)
				}
			}
			var outcome string
			var closed time.Time
			if err := s.db.QueryRowContext(ctx, `SELECT outcome, closed_at FROM prs WHERE repo = 'o/r' AND number = 1`).Scan(&outcome, &closed); err != nil {
				t.Fatal(err)
			}
			if outcome != tc.want || !closed.Equal(t0.Add(time.Minute)) {
				t.Fatalf("late snapshot replaced confirmed merge: outcome = %q, closed = %s", outcome, closed)
			}
		})
	}
}

func TestAppMergeCanRefinePreviouslyRecordedClosure(t *testing.T) {
	for _, previous := range []string{PRClosed, PRMerged} {
		t.Run(previous, func(t *testing.T) {
			s, ctx := open(t), context.Background()
			if err := s.InstallationAdded(ctx, 1, "o", t0); err != nil {
				t.Fatal(err)
			}
			if err := s.SeenPR(ctx, 1, "o/r", 1, "update", t0, t0); err != nil {
				t.Fatal(err)
			}
			if err := s.ClosePR(ctx, "o/r", 1, previous, t0.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			// A delayed audit can establish both attribution and the actual
			// merge time after another run first noticed the PR had closed.
			if err := s.ClosePR(ctx, "o/r", 1, PRMergedByApp, t0.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			m, err := s.Metrics(ctx, t0.Add(time.Hour))
			if err != nil || len(m) != 1 || m[0].MergedByApp != 1 || m[0].MergeTimeApp != time.Minute {
				t.Fatalf("merge attribution = %+v, %v", m, err)
			}
		})
	}
}
