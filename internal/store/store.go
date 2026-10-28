// Package store keeps what the hosted App must remember between runs, in
// SQLite: its installations and when they came and went, each run, the
// "Dependency health" issue of each repository, and every write the App
// made, so that "why did it do that?" always has an answer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS installations (
	id         INTEGER PRIMARY KEY,
	account    TEXT NOT NULL,
	added_at   TIMESTAMP NOT NULL,
	removed_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS runs (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	installation_id INTEGER NOT NULL,
	repo            TEXT NOT NULL,
	trigger         TEXT NOT NULL,
	started_at      TIMESTAMP NOT NULL,
	finished_at     TIMESTAMP,
	error           TEXT NOT NULL DEFAULT '',
	prs             INTEGER NOT NULL DEFAULT 0,
	would_merge     INTEGER NOT NULL DEFAULT 0,
	findings        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS runs_repo ON runs (repo, started_at);
CREATE TABLE IF NOT EXISTS issues (
	repo   TEXT PRIMARY KEY,
	number INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS prs (
	installation_id INTEGER NOT NULL,
	repo            TEXT NOT NULL,
	number          INTEGER NOT NULL,
	title           TEXT NOT NULL DEFAULT '',
	opened_at       TIMESTAMP NOT NULL,
	first_seen_at   TIMESTAMP NOT NULL,
	last_seen_at    TIMESTAMP NOT NULL,
	closed_at       TIMESTAMP,
	outcome         TEXT NOT NULL DEFAULT 'open',
	PRIMARY KEY (repo, number)
);
CREATE TABLE IF NOT EXISTS repos (
	repo            TEXT PRIMARY KEY,
	installation_id INTEGER NOT NULL,
	mode            TEXT NOT NULL,
	merging         INTEGER NOT NULL,
	first_seen_at   TIMESTAMP NOT NULL,
	merge_since     TIMESTAMP,
	updated_at      TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS reviews (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	installation_id   INTEGER NOT NULL,
	repo              TEXT NOT NULL,
	number            INTEGER NOT NULL,
	model             TEXT NOT NULL,
	verdict           TEXT NOT NULL,
	requests          INTEGER NOT NULL,
	prompt_tokens     INTEGER NOT NULL,
	completion_tokens INTEGER NOT NULL,
	seconds           REAL NOT NULL,
	at                TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS reviews_day ON reviews (installation_id, at);
CREATE TABLE IF NOT EXISTS pauses (
	installation_id INTEGER PRIMARY KEY,
	paused_at       TIMESTAMP NOT NULL,
	reason          TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS writes (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES runs (id),
	repo   TEXT NOT NULL,
	kind   TEXT NOT NULL,
	number INTEGER NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	at     TIMESTAMP NOT NULL
);
`

// Open opens or creates the database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One connection: SQLite writes one at a time anyway, and this keeps
	// the busy timeout from ever being needed within the process.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// InstallationAdded records an installation as active, keeping when it
// was first added.
func (s *Store) InstallationAdded(ctx context.Context, id int64, account string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO installations (id, account, added_at) VALUES (?, ?, ?)
ON CONFLICT (id) DO UPDATE SET account = excluded.account, removed_at = NULL`, id, account, at.UTC())
	return err
}

// InstallationRemoved records an installation as gone (uninstalled or
// suspended).
func (s *Store) InstallationRemoved(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE installations SET removed_at = ? WHERE id = ? AND removed_at IS NULL`, at.UTC(), id)
	return err
}

// Installation is one installation as recorded.
type Installation struct {
	ID        int64
	Account   string
	AddedAt   time.Time
	RemovedAt *time.Time
}

// Installations lists every installation ever recorded.
func (s *Store) Installations(ctx context.Context) ([]Installation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account, added_at, removed_at FROM installations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var i Installation
		var removed sql.NullTime
		if err := rows.Scan(&i.ID, &i.Account, &i.AddedAt, &removed); err != nil {
			return nil, err
		}
		if removed.Valid {
			i.RemovedAt = &removed.Time
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// StartRun records a run of repo starting, and returns its id.
func (s *Store) StartRun(ctx context.Context, installation int64, repo, trigger string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (installation_id, repo, trigger, started_at) VALUES (?, ?, ?, ?)`, installation, repo, trigger, at.UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RunOutcome is what a run found.
type RunOutcome struct {
	Error      string
	PRs        int
	WouldMerge int
	Findings   int
}

func (s *Store) FinishRun(ctx context.Context, id int64, o RunOutcome, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET finished_at = ?, error = ?, prs = ?, would_merge = ?, findings = ? WHERE id = ?`,
		at.UTC(), o.Error, o.PRs, o.WouldMerge, o.Findings, id)
	return err
}

// IssueNumber is the number of repo's "Dependency health" issue, or 0
// when none is known.
func (s *Store) IssueNumber(ctx context.Context, repo string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT number FROM issues WHERE repo = ?`, repo).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (s *Store) SetIssueNumber(ctx context.Context, repo string, n int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO issues (repo, number) VALUES (?, ?) ON CONFLICT (repo) DO UPDATE SET number = excluded.number`, repo, n)
	return err
}

// Write is one write the App made to GitHub.
type Write struct {
	RunID  int64
	Repo   string
	Kind   string // such as issue-created, issue-updated
	Number int    // the issue or PR written to
	Detail string
	At     time.Time
}

func (s *Store) RecordWrite(ctx context.Context, w Write) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO writes (run_id, repo, kind, number, detail, at) VALUES (?, ?, ?, ?, ?, ?)`,
		w.RunID, w.Repo, w.Kind, w.Number, w.Detail, w.At.UTC())
	return err
}

// Writes lists the writes made since since, oldest first.
func (s *Store) Writes(ctx context.Context, since time.Time) ([]Write, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, repo, kind, number, detail, at FROM writes WHERE at >= ? ORDER BY id`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Write
	for rows.Next() {
		var w Write
		if err := rows.Scan(&w.RunID, &w.Repo, &w.Kind, &w.Number, &w.Detail, &w.At); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Pause stops installation's writes, other than to its issues, from the
// next one on: the kill switch.
func (s *Store) Pause(ctx context.Context, installation int64, reason string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pauses (installation_id, paused_at, reason) VALUES (?, ?, ?)
ON CONFLICT (installation_id) DO UPDATE SET reason = excluded.reason`, installation, at.UTC(), reason)
	return err
}

// Resume lets installation write again.
func (s *Store) Resume(ctx context.Context, installation int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pauses WHERE installation_id = ?`, installation)
	return err
}

// Paused reports whether installation's writes are paused.
func (s *Store) Paused(ctx context.Context, installation int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pauses WHERE installation_id = ?`, installation).Scan(&n)
	return n > 0, err
}

// The outcomes of a Dependabot PR.
const (
	PROpen        = "open"
	PRMergedByApp = "merged-by-app"
	PRMerged      = "merged" // by anyone else
	PRClosed      = "closed" // without merging
)

// SeenPR records a Dependabot PR open in repo at seen. A PR recorded
// merged stays so: a merge is final, and the read that saw it open is
// older than the one that saw it merged.
func (s *Store) SeenPR(ctx context.Context, installation int64, repo string, number int, title string, opened, seen time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO prs (installation_id, repo, number, title, opened_at, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo, number) DO UPDATE SET title = excluded.title, last_seen_at = excluded.last_seen_at,
	outcome = 'open', closed_at = NULL
WHERE prs.outcome NOT IN ('merged', 'merged-by-app')`, installation, repo, number, title, opened.UTC(), seen.UTC(), seen.UTC())
	return err
}

// ClosePR records how a PR left: one of the PR outcomes other than open.
func (s *Store) ClosePR(ctx context.Context, repo string, number int, outcome string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE prs SET outcome = ?, closed_at = ? WHERE repo = ? AND number = ?`, outcome, at.UTC(), repo, number)
	return err
}

// AppMerge reports when the App last merged repo's PR number, as its
// writes record: a merge whose call failed may have gone through.
func (s *Store) AppMerge(ctx context.Context, repo string, number int) (time.Time, bool, error) {
	var at time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT at FROM writes WHERE repo = ? AND number = ? AND kind IN ('merge', 'merge-attempted') ORDER BY at DESC LIMIT 1`, repo, number).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return at, err == nil, err
}

// OpenPRs lists the numbers of repo's PRs last recorded open.
func (s *Store) OpenPRs(ctx context.Context, repo string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT number FROM prs WHERE repo = ? AND outcome = 'open' ORDER BY number`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SeenRepo records the mode repo ran in, and whether the App could merge
// there. merge_since keeps when merging was first on.
func (s *Store) SeenRepo(ctx context.Context, installation int64, repo, mode string, merging bool, at time.Time) error {
	var since any
	if merging {
		since = at.UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO repos (repo, installation_id, mode, merging, first_seen_at, merge_since, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo) DO UPDATE SET installation_id = excluded.installation_id, mode = excluded.mode, merging = excluded.merging,
	merge_since = COALESCE(repos.merge_since, excluded.merge_since), updated_at = excluded.updated_at`,
		repo, installation, mode, merging, at.UTC(), since, at.UTC())
	return err
}

// ForgetInstallation deletes what was recorded about installation's
// repositories: their runs, writes, issues, PRs, modes and pause. The
// installation itself is kept, with when it came and went, which is all
// the test's retention numbers need.
func (s *Store) ForgetInstallation(ctx context.Context, installation int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM issues WHERE repo IN (SELECT repo FROM runs WHERE installation_id = ?1 UNION SELECT repo FROM repos WHERE installation_id = ?1)`,
		`DELETE FROM writes WHERE run_id IN (SELECT id FROM runs WHERE installation_id = ?1)`,
		`DELETE FROM runs WHERE installation_id = ?1`,
		`DELETE FROM prs WHERE installation_id = ?1`,
		`DELETE FROM repos WHERE installation_id = ?1`,
		`DELETE FROM pauses WHERE installation_id = ?1`,
		`DELETE FROM reviews WHERE installation_id = ?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, installation); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Review is one agent review: the PR it judged, its verdict (approve,
// reject, or failed: and why), and the model usage.
type Review struct {
	Installation     int64
	Repo             string
	Number           int
	Model            string
	Verdict          string
	Requests         int
	PromptTokens     int
	CompletionTokens int
	Took             time.Duration
	At               time.Time
}

func (s *Store) RecordReview(ctx context.Context, r Review) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO reviews (installation_id, repo, number, model, verdict, requests, prompt_tokens, completion_tokens, seconds, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.Installation, r.Repo, r.Number, r.Model, r.Verdict, r.Requests,
		r.PromptTokens, r.CompletionTokens, r.Took.Seconds(), r.At.UTC())
	return err
}

// ReviewsSince counts installation's reviews since since: its daily
// allowance is measured with it.
func (s *Store) ReviewsSince(ctx context.Context, installation int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reviews WHERE installation_id = ? AND at >= ?`, installation, since.UTC()).Scan(&n)
	return n, err
}
