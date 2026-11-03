package store

import (
	"context"
	"time"

	"github.com/sglavoie/deps-handler/internal/jobs"
)

// AppendJob commits an event before its webhook is acknowledged.
func (s *Store) AppendJob(ctx context.Context, job jobs.Job, due time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO queued_jobs (installation_id, repo, trigger, due_at) VALUES (?, ?, ?, ?)`,
		job.Installation, job.Repo, job.Trigger, due.UTC())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// PendingJobs replays every accepted event that has not been acknowledged.
func (s *Store) PendingJobs(ctx context.Context) ([]jobs.SavedJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, installation_id, repo, trigger, due_at FROM queued_jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jobs.SavedJob
	for rows.Next() {
		var saved jobs.SavedJob
		if err := rows.Scan(&saved.ID, &saved.Job.Installation, &saved.Job.Repo, &saved.Job.Trigger, &saved.Due); err != nil {
			return nil, err
		}
		out = append(out, saved)
	}
	return out, rows.Err()
}

// DeleteJobs acknowledges only the events covered by a completed run.
// Events received while that run was in progress have different IDs.
func (s *Store) DeleteJobs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM queued_jobs WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceJobs atomically narrows a completed installation sweep to only
// the repositories that failed. A cleanup that removed the original job
// wins, so a late sweep cannot recreate jobs for an uninstalled App.
func (s *Store) ReplaceJobs(ctx context.Context, ids []int64, replacements []jobs.SavedJob) ([]int64, error) {
	if len(ids) == 0 || len(replacements) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	removed := int64(0)
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `DELETE FROM queued_jobs WHERE id = ?`, id)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		removed += n
	}
	if removed == 0 {
		return nil, nil
	}
	newIDs := make([]int64, 0, len(replacements))
	for _, entry := range replacements {
		result, err := tx.ExecContext(ctx, `INSERT INTO queued_jobs (installation_id, repo, trigger, due_at) VALUES (?, ?, ?, ?)`,
			entry.Job.Installation, entry.Job.Repo, entry.Job.Trigger, entry.Due.UTC())
		if err != nil {
			return nil, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		newIDs = append(newIDs, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return newIDs, nil
}
