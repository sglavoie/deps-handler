package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/sglavoie/deps-merger/internal/store"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/review"
	"github.com/sglavoie/deps-merger/pkg/runner"
)

// Review configures agent review. It runs only in repositories whose
// policy says agent_review: true: the App's base policy says false.
type Review struct {
	// Agent builds the agent for one review, telling usage the tokens it
	// used: review.API with its Usage set.
	Agent func(usage func(review.Usage)) review.Agent
	// Model names the model, for the records and the report.
	Model string
	// PerDay bounds each installation's reviews over any 24 hours.
	PerDay int
	// Timeout bounds one review, checkout included.
	Timeout time.Duration
	// Dir holds the checkouts and verdicts, per installation.
	Dir string
	// Available, when set, says whether the model answers now. A run
	// whose model does not reviews nothing, so a model on a machine that
	// sleeps spends neither the run's time nor the day's reviews.
	Available func(context.Context) error
	// CheckoutURL, when set, is the remote PRs are fetched from, instead
	// of github.com: for tests.
	CheckoutURL func(owner, name string) string
}

func (r *Review) checkouts(installation int64) string {
	return filepath.Join(r.Dir, "checkouts", strconv.FormatInt(installation, 10))
}

func (r *Review) verdicts(installation int64) string {
	return filepath.Join(r.Dir, "verdicts", strconv.FormatInt(installation, 10)+".json")
}

// setupReview gives the run cached verdicts even when no new reviews are
// available. New reviews check PRs out with the installation's token.
func (s *Service) setupReview(ctx context.Context, log *slog.Logger, r *runner.Runner, engine github.Client, installation int64, repo string) error {
	rv := s.Review
	done, err := s.Store.ReviewsSince(ctx, installation, time.Now().Add(-24*time.Hour))
	if err != nil {
		log.Error("count reviews", "err", err)
		return fmt.Errorf("count reviews: %w", err)
	}
	verdicts, err := review.OpenCache(rv.verdicts(installation))
	if err != nil {
		log.Error("no agent review", "err", err)
		return fmt.Errorf("open verdict cache: %w", err)
	}
	r.AgentName, r.Verdicts = rv.Model, verdicts
	left := rv.PerDay - done
	if left <= 0 {
		log.Info("no agent review: the installation's reviews for the day are used", "per_day", rv.PerDay)
		return nil
	}
	token, err := s.Platform.Token(ctx, installation)
	if err != nil {
		log.Error("no agent review", "err", err)
		return fmt.Errorf("installation checkout token: %w", err)
	}
	r.Reviewer = &review.AgentReviewer{
		Agent:   &metered{s: s, log: log, installation: installation, repo: repo},
		Details: engine,
		Workspace: &review.Workspace{Root: rv.checkouts(installation), Token: token, URL: rv.CheckoutURL,
			TokenSource: func(ctx context.Context) (string, error) { return s.Platform.Token(ctx, installation) }},
		Timeout: rv.Timeout,
	}
	if rv.Available != nil {
		if err := rv.Available(ctx); err != nil {
			// Verdicts already given still count; a review not yet given
			// waits for the model, without a checkout or the day's reviews.
			log.Info("agent review unavailable", "err", err)
			r.Reviewer = unavailable{}
		}
	}
	// One at a time: a local model serves one request at a time anyway.
	r.MaxReviews, r.ReviewParallel = left, 1
	return nil
}

// unavailable is the reviewer while the model does not answer.
type unavailable struct{}

func (unavailable) Review(context.Context, review.Request) (policy.Verdict, error) {
	return policy.Verdict{}, errors.New("the model is unavailable; the next run tries again")
}

// metered is the agent of one run, recording each review it does with
// the tokens it used.
type metered struct {
	s            *Service
	log          *slog.Logger
	installation int64
	repo         string
}

func (m *metered) Name() string { return m.s.Review.Model }

var prNumber = regexp.MustCompile(`Pull request #(\d+)`)

func (m *metered) Run(ctx context.Context, dir, prompt string) (review.Answer, error) {
	var used review.Usage
	started := time.Now()
	ans, err := m.s.Review.Agent(func(u review.Usage) { used = u }).Run(ctx, dir, prompt)
	verdict := ans.Decision
	if err != nil {
		verdict = "failed: " + review.Clean(err.Error())
	}
	n := 0
	if match := prNumber.FindStringSubmatch(prompt); match != nil {
		n, _ = strconv.Atoi(match[1])
	}
	rec := store.Review{Installation: m.installation, Repo: m.repo, Number: n, Model: m.s.Review.Model, Verdict: verdict,
		Requests: used.Requests, PromptTokens: used.PromptTokens, CompletionTokens: used.CompletionTokens,
		Took: time.Since(started), At: time.Now()}
	if rerr := m.s.Store.RecordReview(context.WithoutCancel(ctx), rec); rerr != nil {
		m.log.Error("record review", "err", rerr)
		err = errors.Join(err, fmt.Errorf("record review: %w", rerr))
	}
	m.log.Info("agent review", "pr", n, "verdict", verdict, "prompt_tokens", used.PromptTokens,
		"completion_tokens", used.CompletionTokens, "took", time.Since(started).Round(time.Second).String())
	return ans, err
}

// Forget deletes what is kept about installation's repositories: its
// records, checkouts and cached verdicts. The installation itself stays.
func (s *Service) Forget(ctx context.Context, installation int64) error {
	g := s.gate(installation)
	g.run.Lock()
	defer g.run.Unlock()
	g.write.Lock()
	err := s.Store.ForgetInstallation(ctx, installation)
	g.write.Unlock()
	if err != nil {
		return err
	}
	return s.forgetReviewFiles(installation)
}

// forgetIfInactiveBefore is used after a removal. An installation webhook
// that reactivated it after the removal or sweep snapshot keeps its records.
func (s *Service) forgetIfInactiveBefore(ctx context.Context, installation int64, observedAt time.Time) error {
	g := s.gate(installation)
	g.run.Lock()
	defer g.run.Unlock()
	g.write.Lock()
	if g.lastEvent.After(observedAt) {
		g.write.Unlock()
		return nil
	}
	inactive, err := s.Store.InstallationInactive(ctx, installation)
	if err == nil && inactive {
		err = s.Store.ForgetInstallation(ctx, installation)
	}
	g.write.Unlock()
	if err != nil || !inactive {
		return err
	}
	return s.forgetReviewFiles(installation)
}

func (s *Service) forgetReviewFiles(installation int64) error {
	if s.Review == nil {
		return nil
	}
	if err := os.RemoveAll(s.Review.checkouts(installation)); err != nil {
		return fmt.Errorf("checkouts: %w", err)
	}
	if err := os.Remove(s.Review.verdicts(installation)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("verdicts: %w", err)
	}
	return nil
}
