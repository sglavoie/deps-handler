package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenMigratesInstallationObservations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE installations (
id INTEGER PRIMARY KEY, account TEXT NOT NULL, added_at TIMESTAMP NOT NULL, removed_at TIMESTAMP);
PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 2; id++ {
		var removed any
		if id == 2 {
			removed = t0.Add(time.Hour)
		}
		if _, err := legacy.ExecContext(ctx, `INSERT INTO installations VALUES (?, 'o', ?, ?)`, id, t0, removed); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	for id := 1; id <= 2; id++ {
		var observed time.Time
		want := t0.Add(time.Duration(id-1) * time.Hour)
		if err := s.db.QueryRowContext(ctx, `SELECT observed_at FROM installations WHERE id = ?`, id).Scan(&observed); err != nil || !observed.Equal(want) {
			t.Fatalf("installation %d observation = %v, %v; want %v", id, observed, err, want)
		}
	}
	// The migrated removal must still reject an older active observation.
	if err := s.InstallationAdded(ctx, 2, "stale", t0); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.InstallationInactive(ctx, 2); err != nil || !inactive {
		t.Fatalf("migration lost removal guard: %t, %v", inactive, err)
	}
	latest := t0.Add(2 * time.Hour)
	if err := s.InstallationAdded(ctx, 2, "current", latest); err != nil {
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
	if err := s.InstallationRemoved(ctx, 2, latest.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.InstallationInactive(ctx, 2); err != nil || inactive {
		t.Fatalf("reopening lost latest observation: %t, %v", inactive, err)
	}
}

func TestRepeatedInactiveObservationRejectsIntermediateReactivation(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.InstallationRemoved(ctx, 7, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallationSuspended(ctx, 7, "current", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallationAdded(ctx, 7, "stale", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ins, err := s.Installations(ctx)
	if err != nil || len(ins) != 1 || ins[0].RemovedAt == nil || !ins[0].RemovedAt.Equal(t0) || ins[0].Account != "current" {
		t.Fatalf("newest state and first removal must both survive: %+v, %v", ins, err)
	}
}

func TestForgetInstallationBeforePreservesNewerObservations(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.InstallationRemoved(ctx, 7, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SeenPR(ctx, 7, "o/r", 1, "update", t0, t0); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := s.ForgetInstallationBefore(ctx, 7, t0.Add(-time.Minute)); err != nil || forgotten {
		t.Fatalf("old cleanup = %t, %v", forgotten, err)
	}
	if prs, err := s.OpenPRs(ctx, "o/r"); err != nil || len(prs) != 1 {
		t.Fatalf("old cleanup deleted records: %v, %v", prs, err)
	}
	if forgotten, err := s.ForgetInstallationBefore(ctx, 7, t0); err != nil || !forgotten {
		t.Fatalf("current cleanup = %t, %v", forgotten, err)
	}
	if prs, err := s.OpenPRs(ctx, "o/r"); err != nil || len(prs) != 0 {
		t.Fatalf("current cleanup kept records: %v, %v", prs, err)
	}
}
