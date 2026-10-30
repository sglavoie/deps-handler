package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// RunSnapshot is the repository and PR evidence collected by one run.
// GitHub reads finish before this snapshot is committed, so no network
// request holds SQLite's write lock.
type RunSnapshot struct {
	Mode     string
	Merging  bool
	Complete bool
	At       time.Time
	PRs      []PRObservation
	Closed   []PRClosure
}

// PRObservation is a PR the run saw open, including one it later merged.
type PRObservation struct {
	Number   int
	Title    string
	OpenedAt time.Time
}

// PRClosure is the confirmed outcome of a PR no longer open.
type PRClosure struct {
	Number  int
	Outcome string
	At      time.Time
}

// RecordRunSnapshot atomically records evidence only if the original run
// still exists. Cleanup deletes that run in the same transaction as its
// repository data; a late snapshot must not restore any of it, even after
// the installation is reactivated. Returns false when cleanup won.
func (s *Store) RecordRunSnapshot(ctx context.Context, runID int64, snapshot RunSnapshot) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var installation int64
	var repo string
	// Acquire the write lock while checking the run, before writing its
	// evidence. Cleanup must either precede this check or follow commit.
	err = tx.QueryRowContext(ctx, `UPDATE runs SET id = id WHERE id = ? RETURNING installation_id, repo`, runID).Scan(&installation, &repo)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if snapshot.Complete {
		if err := seenRepo(ctx, tx, installation, repo, snapshot.Mode, snapshot.Merging, snapshot.At); err != nil {
			return false, fmt.Errorf("track repository: %w", err)
		}
	}
	for _, pr := range snapshot.PRs {
		if err := seenPR(ctx, tx, installation, repo, pr.Number, pr.Title, pr.OpenedAt, snapshot.At); err != nil {
			return false, fmt.Errorf("track PR #%d: %w", pr.Number, err)
		}
	}
	for _, pr := range snapshot.Closed {
		if err := closePR(ctx, tx, repo, pr.Number, pr.Outcome, pr.At); err != nil {
			return false, fmt.Errorf("track closed PR #%d: %w", pr.Number, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
