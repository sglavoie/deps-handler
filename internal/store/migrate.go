package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// GitHub treats repository names without regard to case. Keep one key in
// every table, including databases written before repoKey was introduced.
func repoKey(repo string) string { return strings.ToLower(repo) }

func migrateRepoKeys(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 1 {
		return nil
	}
	// Prefer a confirmed merge if old case variants disagree about a PR.
	// Otherwise keep the most recently observed state. Preserve its earliest
	// opening and first sighting, which the metrics use for age calculations.
	statements := []string{
		`CREATE TEMP TABLE pr_history AS SELECT lower(repo) AS repo, number,
			MIN(opened_at) AS opened_at, MIN(first_seen_at) AS first_seen_at,
			MAX(last_seen_at) AS last_seen_at FROM prs GROUP BY lower(repo), number`,
		`DELETE FROM prs WHERE rowid NOT IN (
			SELECT rowid FROM (SELECT rowid, ROW_NUMBER() OVER (
				PARTITION BY lower(repo), number ORDER BY
				CASE outcome WHEN 'merged-by-app' THEN 2 WHEN 'merged' THEN 1 ELSE 0 END DESC,
				COALESCE(closed_at, last_seen_at) DESC, rowid DESC) AS rank FROM prs)
			WHERE rank = 1)`,
		`UPDATE prs SET repo = lower(repo),
			opened_at = (SELECT opened_at FROM pr_history h WHERE h.repo = lower(prs.repo) AND h.number = prs.number),
			first_seen_at = (SELECT first_seen_at FROM pr_history h WHERE h.repo = lower(prs.repo) AND h.number = prs.number),
			last_seen_at = (SELECT last_seen_at FROM pr_history h WHERE h.repo = lower(prs.repo) AND h.number = prs.number)`,
		`DROP TABLE pr_history`,
		`CREATE TEMP TABLE repo_history AS SELECT lower(repo) AS repo,
			MIN(first_seen_at) AS first_seen_at, MIN(merge_since) AS merge_since
			FROM repos GROUP BY lower(repo)`,
		`DELETE FROM repos WHERE rowid NOT IN (
			SELECT rowid FROM (SELECT rowid, ROW_NUMBER() OVER (
				PARTITION BY lower(repo) ORDER BY updated_at DESC, rowid DESC) AS rank FROM repos)
			WHERE rank = 1)`,
		`UPDATE repos SET repo = lower(repo),
			first_seen_at = (SELECT first_seen_at FROM repo_history h WHERE h.repo = lower(repos.repo)),
			merge_since = (SELECT merge_since FROM repo_history h WHERE h.repo = lower(repos.repo))`,
		`DROP TABLE repo_history`,
		`DELETE FROM issues WHERE rowid NOT IN (
			SELECT rowid FROM (SELECT rowid, ROW_NUMBER() OVER (
				PARTITION BY lower(repo) ORDER BY number DESC, rowid DESC) AS rank FROM issues)
			WHERE rank = 1)`,
		`UPDATE issues SET repo = lower(repo)`,
		`UPDATE repo_access SET repo = lower(repo)`,
		`UPDATE runs SET repo = lower(repo)`,
		`UPDATE writes SET repo = lower(repo)`,
		`UPDATE reviews SET repo = lower(repo)`,
		`PRAGMA user_version = 1`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("normalize repository keys: %w", err)
		}
	}
	return tx.Commit()
}

// Keep installation observations independent of added_at and removed_at:
// those retain the first installation and removal times for metrics.
func migrateInstallationObservations(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 2 {
		return nil
	}
	var columns int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('installations') WHERE name = 'observed_at'`).Scan(&columns); err != nil {
		return err
	}
	if columns == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE installations ADD COLUMN observed_at TIMESTAMP NOT NULL DEFAULT '0001-01-01 00:00:00 +0000 UTC'`); err != nil {
			return err
		}
		// Earlier schemas retained only these event times. Preserve the
		// newest known one when seeding the observation guard.
		if _, err := tx.ExecContext(ctx, `UPDATE installations SET observed_at = COALESCE(removed_at, added_at)`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 2`); err != nil {
		return err
	}
	return tx.Commit()
}
