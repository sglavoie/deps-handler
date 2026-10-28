// Package service runs deps-merger's engine for the App's installations:
// one repository at a time, with every write it makes to GitHub guarded
// and recorded.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-merger/internal/app"
	"github.com/sglavoie/deps-merger/internal/jobs"
	"github.com/sglavoie/deps-merger/internal/output"
	"github.com/sglavoie/deps-merger/internal/store"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/runner"
)

// Platform is GitHub as the App sees it; *app.App is the real one.
type Platform interface {
	Login() string
	Installations(ctx context.Context) ([]app.Installation, error)
	Repos(ctx context.Context, installation int64) ([]string, error)
	// Engine is a deps-merger client acting as installation.
	Engine(ctx context.Context, installation int64) (github.Client, error)
	// REST is a go-github client acting as installation, for its issues.
	REST(installation int64) (*gh.Client, error)
	// Token is an access token of installation, for checkouts.
	Token(ctx context.Context, installation int64) (string, error)
}

// Service runs repositories for the App.
type Service struct {
	Platform Platform
	Store    *store.Store
	Log      *slog.Logger
	// Allowed, when set, are the only repositories run.
	Allowed []string
	// AllowMerge lets the engine merge, in repositories whose policy says
	// mode: merge, for installations not paused. Without it the App only
	// reports.
	AllowMerge bool
	// Sleep waits between the engine's polls after a merge; nil waits.
	Sleep func(context.Context, time.Duration) error
	// Review, when set, lets an agent judge updates the policy alone would
	// not merge, in repositories that ask for it with agent_review: true.
	Review *Review
}

// Base is the policy a repository gets before its own files: report
// mode, no Dependabot commands (Dependabot ignores an App's), and no
// agent review, which is not offered yet.
func Base() config.Policy {
	p := config.Builtin
	p.Mode, p.DependabotCommands, p.AgentReview = config.ModeReport, false, false
	return p
}

// runs reports whether repo may run, and logs it when it may not.
func (s *Service) runs(repo string) bool {
	if len(s.Allowed) == 0 || slices.ContainsFunc(s.Allowed, func(a string) bool { return strings.EqualFold(a, repo) }) {
		return true
	}
	s.Log.Info("repository not in DMC_REPOS; skipped", "repo", repo)
	return false
}

// SweepEvery queues every installation now and then every period, keeping
// the record of installations in step with GitHub's.
func (s *Service) SweepEvery(ctx context.Context, period time.Duration, enqueue func(jobs.Job)) {
	for {
		if ids, err := s.SyncInstallations(ctx); err != nil {
			s.Log.Error("sweep", "err", err)
		} else {
			for _, id := range ids {
				enqueue(jobs.Job{Installation: id, Trigger: "schedule"})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(period):
		}
	}
}

// SyncInstallations records the installations GitHub lists as active,
// and returns them. One it no longer lists was uninstalled, its webhook
// perhaps missed: it is recorded as removed, and what was kept about its
// repositories is forgotten, as on the webhook. A suspended one is
// listed, and only stops running.
func (s *Service) SyncInstallations(ctx context.Context) ([]int64, error) {
	ins, err := s.Platform.Installations(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var active []int64
	for _, in := range ins {
		if in.Suspended {
			continue
		}
		active = append(active, in.ID)
		if err := s.Store.InstallationAdded(ctx, in.ID, in.Account, now); err != nil {
			return nil, err
		}
	}
	known, err := s.Store.Installations(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range known {
		if k.RemovedAt == nil && !slices.Contains(active, k.ID) {
			if err := s.Store.InstallationRemoved(ctx, k.ID, now); err != nil {
				return nil, err
			}
		}
		if !slices.ContainsFunc(ins, func(in app.Installation) bool { return in.ID == k.ID }) {
			if err := s.Forget(ctx, k.ID); err != nil {
				return nil, err
			}
		}
	}
	return active, nil
}

// Once runs every repository of every installation, or only those named,
// one after the other.
func (s *Service) Once(ctx context.Context, only []string) error {
	ids, err := s.SyncInstallations(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		repos, err := s.Platform.Repos(ctx, id)
		if err != nil {
			return err
		}
		for _, r := range repos {
			if len(only) == 0 || slices.ContainsFunc(only, func(o string) bool { return strings.EqualFold(o, r) }) {
				if s.runs(r) {
					s.RunRepo(ctx, id, r, "once")
				}
			}
		}
	}
	return ctx.Err()
}

// Run does a queued job: one repository, or each of the installation's.
func (s *Service) Run(ctx context.Context, j jobs.Job) {
	if j.Repo != "" {
		if s.runs(j.Repo) {
			s.RunRepo(ctx, j.Installation, j.Repo, j.Trigger)
		}
		return
	}
	repos, err := s.Platform.Repos(ctx, j.Installation)
	if err != nil {
		s.Log.Error("list repositories", "installation", j.Installation, "err", err)
		return
	}
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		if s.runs(r) {
			s.RunRepo(ctx, j.Installation, r, j.Trigger)
		}
	}
}

// RunRepo runs the engine on one repository and brings its issue up to
// date. The engine merges only when merging is allowed, the installation
// is not paused, and the repository's policy says mode: merge.
func (s *Service) RunRepo(ctx context.Context, installation int64, repo, trigger string) {
	log := s.Log.With("installation", installation, "repo", repo, "trigger", trigger)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		log.Error("bad repository name")
		return
	}
	started := time.Now()
	runID, err := s.Store.StartRun(ctx, installation, repo, trigger, started)
	if err != nil {
		log.Error("record run", "err", err)
		return
	}
	outcome := store.RunOutcome{}
	defer func() {
		if err := s.Store.FinishRun(context.WithoutCancel(ctx), runID, outcome, time.Now()); err != nil {
			log.Error("record run", "err", err)
		}
	}()

	paused, err := s.Store.Paused(ctx, installation)
	if err != nil {
		outcome.Error = err.Error()
		log.Error("run", "err", err)
		return
	}
	merging := s.AllowMerge && !paused
	engine, err := s.Platform.Engine(ctx, installation)
	if err != nil {
		outcome.Error = err.Error()
		log.Error("run", "err", err)
		return
	}
	client := &guarded{Client: engine, s: s, installation: installation, repo: repo, runID: runID, log: log}
	r := runner.New(client, s.Platform.Login(), merging)
	r.Parallel = 1
	if s.Sleep != nil {
		r.Sleep = s.Sleep
	}
	if s.Review != nil {
		s.setupReview(ctx, log, r, client, installation, repo)
	}
	res := r.RunRepo(ctx, config.NewRepo(owner, name, Base()))

	outcome.Error = res.Error
	outcome.PRs = len(res.Decisions)
	outcome.Findings = len(res.Findings)
	merged := 0
	for _, ev := range res.Events {
		if ev.Kind == runner.EventMerged {
			merged++
		}
	}
	for _, d := range res.Decisions {
		if d.Action == policy.ActionMerge {
			outcome.WouldMerge++
		}
	}
	repoMerges := merging && res.Policy.Mode == config.ModeMerge
	// What a failed or interrupted run read and merged happened all the
	// same: it is recorded, past the run's end if need be.
	complete := res.Error == "" && ctx.Err() == nil
	s.track(context.WithoutCancel(ctx), log, engine, installation, repo, res, client.seen, client.merged, repoMerges, complete)
	if ctx.Err() != nil {
		return
	}

	rest, err := s.Platform.REST(installation)
	if err != nil {
		log.Error("issue", "err", err)
		return
	}
	known, err := s.Store.IssueNumber(ctx, repo)
	if err != nil {
		log.Error("issue", "err", err)
		return
	}
	n, change, err := output.Sync(ctx, rest, s.Platform.Login(), owner, name, known, output.Body(res, repoMerges), output.Wanted(res), res.Error == "")
	if n != known && n != 0 {
		if err := s.Store.SetIssueNumber(ctx, repo, n); err != nil {
			log.Error("record issue", "err", err)
		}
	}
	if change != output.Unchanged {
		s.record(ctx, log, store.Write{RunID: runID, Repo: repo, Kind: string(change), Number: n})
	}
	if errors.Is(err, output.ErrIssuesDisabled) {
		log.Info("issues are disabled; nothing to report to")
		return
	}
	if err != nil {
		log.Error("issue", "err", err)
		if outcome.Error == "" {
			outcome.Error = "issue: " + err.Error()
		}
		return
	}
	log.Info("run done", "prs", outcome.PRs, "would_merge", outcome.WouldMerge, "merged", merged, "findings", outcome.Findings,
		"merging", repoMerges, "issue", n, "change", string(change), "error", res.Error, "took", time.Since(started).Round(time.Millisecond).String())
}

// track records the repository's mode and its Dependabot PRs as res saw
// them, for the test's numbers: each open PR, each the App merged, and
// how each PR open last time but not now left. The engine is asked about
// those, as merged or closed, and a merge the App's writes record is the
// App's. Only a complete run records the mode or judges which PRs left,
// and none when the PRs read were cut short (over 100).
func (s *Service) track(ctx context.Context, log *slog.Logger, engine github.Client, installation int64, repo string, res runner.RepoResult, read map[int]github.PullRequest, mergedAt map[int]time.Time, merging, complete bool) {
	now := time.Now()
	if complete {
		if err := s.Store.SeenRepo(ctx, installation, repo, res.Policy.Mode, merging, now); err != nil {
			log.Error("track repository", "err", err)
		}
	}
	before, err := s.Store.OpenPRs(ctx, repo)
	if err != nil {
		log.Error("track PRs", "err", err)
		return
	}
	// Every PR the run read, those it merged part way included, was open
	// then; those in its last read are open still.
	for _, pr := range read {
		opened := pr.CreatedAt
		if opened.IsZero() {
			opened = now
		}
		if err := s.Store.SeenPR(ctx, installation, repo, pr.Number, pr.Title, opened, now); err != nil {
			log.Error("track PRs", "err", err)
		}
	}
	seen := map[int]bool{}
	for _, d := range res.Decisions {
		seen[d.Number] = true
	}
	// The App's merges, timed when they happened; one whose call failed
	// yet went through, as the engine found, when the run saw it.
	byApp := map[int]time.Time{}
	for _, ev := range res.Events {
		if ev.Kind == runner.EventMerged {
			byApp[ev.Number] = now
		}
	}
	maps.Copy(byApp, mergedAt)
	for n, at := range byApp {
		if err := s.Store.ClosePR(ctx, repo, n, store.PRMergedByApp, at); err != nil {
			log.Error("track PRs", "err", err)
		}
	}
	if !complete {
		return
	}
	if slices.ContainsFunc(res.Findings, func(f policy.Finding) bool { return f.Kind == policy.FindingTruncated }) {
		return
	}
	owner, name, _ := strings.Cut(repo, "/")
	for _, n := range before {
		if _, merged := byApp[n]; seen[n] || merged {
			continue
		}
		state, err := engine.PullRequestState(ctx, owner, name, n)
		if err != nil {
			log.Info("PR left, state unknown", "pr", n, "err", err)
			continue
		}
		outcome, at := store.PRClosed, now
		switch state {
		case github.PROpen:
			// No longer a Dependabot PR the search finds, yet open.
			continue
		case github.PRMerged:
			outcome = store.PRMerged
			// Merged by a run that could not record it.
			if t, ok, err := s.Store.AppMerge(ctx, repo, n); err != nil {
				log.Error("track PRs", "err", err)
			} else if ok {
				outcome, at = store.PRMergedByApp, t
			}
		}
		if err := s.Store.ClosePR(ctx, repo, n, outcome, at); err != nil {
			log.Error("track PRs", "err", err)
		}
	}
}

func (s *Service) record(ctx context.Context, log *slog.Logger, w store.Write) {
	w.At = time.Now()
	if err := s.Store.RecordWrite(context.WithoutCancel(ctx), w); err != nil {
		log.Error("record write", "err", err, "kind", w.Kind, "number", w.Number)
	}
}

// ErrPaused is a merge refused because the installation's writes are
// paused, or merging is not enabled.
var ErrPaused = errors.New("writes are paused for this installation")

// ErrNoComments is a comment the App refused to post: Dependabot ignores
// an App's commands, so the App posts none.
var ErrNoComments = errors.New("the App posts no comments, as Dependabot ignores an App's commands; set dependabot_commands: false")

// guarded is the engine's client for one run: every merge first checks
// that merging is still allowed, the kill switch included, so a pause
// stops a run part way, and is recorded with the head it was pinned to.
// No comment is ever posted.
type guarded struct {
	github.Client
	s            *Service
	installation int64
	repo         string
	runID        int64
	log          *slog.Logger

	mu sync.Mutex
	// seen are the Dependabot PRs the run read, by number.
	seen map[int]github.PullRequest
	// merged are when the run merged each PR it merged: a review can make
	// a run last long after.
	merged map[int]time.Time
}

func (g *guarded) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	r, err := g.Client.FetchRepo(ctx, owner, name)
	if r != nil {
		g.mu.Lock()
		if g.seen == nil {
			g.seen = map[int]github.PullRequest{}
		}
		for _, pr := range r.PullRequests {
			g.seen[pr.Number] = pr
		}
		g.mu.Unlock()
	}
	return r, err
}

func (g *guarded) Merge(ctx context.Context, owner, name string, number int, method, headOID string) error {
	paused, err := g.s.Store.Paused(ctx, g.installation)
	switch {
	case err != nil:
		return fmt.Errorf("checking the pause: %w", err)
	case paused || !g.s.AllowMerge:
		g.s.record(ctx, g.log, store.Write{RunID: g.runID, Repo: g.repo, Kind: "merge-refused", Number: number, Detail: "paused"})
		return ErrPaused
	}
	err = g.Client.Merge(ctx, owner, name, number, method, headOID)
	w := store.Write{RunID: g.runID, Repo: g.repo, Kind: "merge", Number: number, Detail: fmt.Sprintf("%s at %.7s", method, headOID)}
	if err != nil {
		// It may have gone through all the same: the engine finds out,
		// and the record says it was attempted.
		w.Kind, w.Detail = "merge-attempted", w.Detail+": "+err.Error()
	} else {
		g.mu.Lock()
		if g.merged == nil {
			g.merged = map[int]time.Time{}
		}
		g.merged[number] = time.Now()
		g.mu.Unlock()
	}
	g.s.record(ctx, g.log, w)
	return err
}

func (g *guarded) Comment(context.Context, string, string, int, string) error {
	return ErrNoComments
}
