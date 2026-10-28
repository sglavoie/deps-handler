package store

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// RetentionDays is the reporting window for whether an installation stayed.
const RetentionDays = 28

// Metrics summarizes one installation's Dependabot activity and App usage.
type Metrics struct {
	Installation
	// Days installed, until now or until it was removed.
	Days int `json:"days"`
	// Retained says whether it was still installed RetentionDays after it
	// was added; nil until that day comes.
	Retained *bool `json:"retained"`

	Repos        int `json:"repos"`
	ReposMerging int `json:"repos_merging"`

	PRs            int `json:"prs"`
	Open           int `json:"open"`
	MergedByApp    int `json:"merged_by_app"`
	MergedOther    int `json:"merged_other"`
	ClosedUnmerged int `json:"closed_unmerged"`
	// AppShare is the share of merged Dependabot PRs the App merged, with
	// no human action; -1 before any merged.
	AppShare float64 `json:"app_share"`

	// OpenAgeAtStart is the median age of the PRs already open when the
	// App first saw them: the "before". OpenAgeNow is the median age of
	// those open now.
	OpenAgeAtStart time.Duration `json:"open_age_at_start"`
	OpenAgeNow     time.Duration `json:"open_age_now"`
	// MergeTimeApp and MergeTimeOther are the median times from opening
	// to merging, by the App and by anyone else.
	MergeTimeApp   time.Duration `json:"merge_time_app"`
	MergeTimeOther time.Duration `json:"merge_time_other"`

	Writes int `json:"writes"`
	Merges int `json:"merges"`

	// Reviews and token counts help track the self-hosted model's usage.
	Reviews          int `json:"reviews"`
	ReviewsApproved  int `json:"reviews_approved"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Metrics computes every installation's numbers as of now.
func (s *Store) Metrics(ctx context.Context, now time.Time) ([]Metrics, error) {
	ins, err := s.Installations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Metrics, 0, len(ins))
	for _, in := range ins {
		m := Metrics{Installation: in, AppShare: -1}
		end := now
		if in.RemovedAt != nil {
			end = *in.RemovedAt
		}
		m.Days = int(end.Sub(in.AddedAt).Hours() / 24)
		if now.Sub(in.AddedAt) >= RetentionDays*24*time.Hour {
			kept := in.RemovedAt == nil || in.RemovedAt.Sub(in.AddedAt) >= RetentionDays*24*time.Hour
			m.Retained = &kept
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(merging), 0) FROM repos WHERE installation_id = ?`, in.ID).
			Scan(&m.Repos, &m.ReposMerging); err != nil {
			return nil, err
		}
		if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(w.kind = 'merge'), 0) FROM writes w JOIN runs r ON r.id = w.run_id WHERE r.installation_id = ?`, in.ID).
			Scan(&m.Writes, &m.Merges); err != nil {
			return nil, err
		}
		if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(verdict = 'approve'), 0), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
FROM reviews WHERE installation_id = ?`, in.ID).Scan(&m.Reviews, &m.ReviewsApproved, &m.PromptTokens, &m.CompletionTokens); err != nil {
			return nil, err
		}
		if err := s.prMetrics(ctx, &m, now); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) prMetrics(ctx context.Context, m *Metrics, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT opened_at, first_seen_at, closed_at, outcome FROM prs WHERE installation_id = ?`, m.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var atStart, openNow, mergeApp, mergeOther []time.Duration
	for rows.Next() {
		var opened, firstSeen time.Time
		var closed sql.NullTime
		var outcome string
		if err := rows.Scan(&opened, &firstSeen, &closed, &outcome); err != nil {
			return err
		}
		m.PRs++
		if opened.Before(m.AddedAt) || opened.Before(firstSeen.Add(-time.Hour)) {
			// Already waiting when the App came.
			atStart = append(atStart, firstSeen.Sub(opened))
		}
		switch outcome {
		case PROpen:
			m.Open++
			openNow = append(openNow, now.Sub(opened))
		case PRMergedByApp:
			m.MergedByApp++
			if closed.Valid {
				mergeApp = append(mergeApp, closed.Time.Sub(opened))
			}
		case PRMerged:
			m.MergedOther++
			if closed.Valid {
				mergeOther = append(mergeOther, closed.Time.Sub(opened))
			}
		case PRClosed:
			m.ClosedUnmerged++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if merged := m.MergedByApp + m.MergedOther; merged > 0 {
		m.AppShare = float64(m.MergedByApp) / float64(merged)
	}
	m.OpenAgeAtStart, m.OpenAgeNow = median(atStart), median(openNow)
	m.MergeTimeApp, m.MergeTimeOther = median(mergeApp), median(mergeOther)
	return nil
}

// median is the middle of ds, or 0 when there are none.
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	slices.Sort(ds)
	if len(ds)%2 == 1 {
		return ds[len(ds)/2]
	}
	return (ds[len(ds)/2-1] + ds[len(ds)/2]) / 2
}
