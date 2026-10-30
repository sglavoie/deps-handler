// Package service runs deps-handler's engine for the App's installations:
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
	"github.com/sglavoie/deps-handler/internal/app"
	"github.com/sglavoie/deps-handler/internal/jobs"
	"github.com/sglavoie/deps-handler/internal/output"
	"github.com/sglavoie/deps-handler/internal/store"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

// Platform is GitHub as the App sees it; *app.App is the real one.
type Platform interface {
	Login() string
	Installations(ctx context.Context) ([]app.Installation, error)
	Repos(ctx context.Context, installation int64) ([]string, error)
	// Engine is a deps-handler client acting as installation.
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
	// ReviewDir locates retained review files when Review is disabled.
	// When Review is set, its Dir is used instead.
	ReviewDir string
	gatesMu   sync.Mutex
	gates     map[int64]*installationGate
}

// An installation's runs and cleanup cannot overlap. Removal may still
// mark it inactive during a run; write serializes that change with GitHub
// writes so no new write starts after removal has been recorded.
type installationGate struct {
	run   sync.Mutex
	write sync.Mutex
	// The cooldown survives individual repository runs and covers both
	// engine writes and the issue client's requests.
	rateMu       sync.Mutex
	retryAt      time.Time
	activeMu     sync.Mutex
	activeRepo   string
	activeCancel context.CancelFunc
}

func (g *installationGate) start(repo string, cancel context.CancelFunc) {
	g.activeMu.Lock()
	g.activeRepo = repo
	g.activeCancel = cancel
	g.activeMu.Unlock()
}

func (g *installationGate) stop() {
	g.activeMu.Lock()
	if g.activeCancel != nil {
		g.activeCancel()
	}
	g.activeMu.Unlock()
}

func (g *installationGate) stopRepo(repo string) {
	g.activeMu.Lock()
	if g.activeCancel != nil && strings.EqualFold(g.activeRepo, repo) {
		g.activeCancel()
	}
	g.activeMu.Unlock()
}

func (g *installationGate) finish() {
	g.activeMu.Lock()
	g.activeRepo = ""
	g.activeCancel = nil
	g.activeMu.Unlock()
}

func (s *Service) gate(id int64) *installationGate {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	if s.gates == nil {
		s.gates = make(map[int64]*installationGate)
	}
	if s.gates[id] == nil {
		s.gates[id] = new(installationGate)
	}
	return s.gates[id]
}

func (s *Service) InstallationAdded(ctx context.Context, id int64, account string) error {
	return s.installationAddedAt(ctx, id, account, time.Now())
}

// SyncInstallationWebhook applies GitHub's current installation state
// instead of a delivery's action. A delayed or redelivered suspend, delete,
// or unsuspend must not undo a newer change. The observation time is taken
// before the API read so a newer observation cannot be overwritten by a
// slower one.
func (s *Service) SyncInstallationWebhook(ctx context.Context, id int64) error {
	observedAt := time.Now()
	installations, err := s.Platform.Installations(ctx)
	if err != nil {
		return fmt.Errorf("list installations: %w", err)
	}
	for _, in := range installations {
		if in.ID != id {
			continue
		}
		if in.Suspended {
			return s.installationSuspendedAt(ctx, id, in.Account, observedAt)
		}
		return s.installationAddedAt(ctx, id, in.Account, observedAt)
	}
	return s.installationRemovedAt(ctx, id, true, observedAt)
}

func (s *Service) installationAddedAt(ctx context.Context, id int64, account string, observedAt time.Time) error {
	_, err := s.observeInstallation(ctx, id, account, false, observedAt)
	return err
}

func (s *Service) installationSuspendedAt(ctx context.Context, id int64, account string, observedAt time.Time) error {
	_, err := s.observeInstallation(ctx, id, account, true, observedAt)
	return err
}

func (s *Service) observeInstallation(ctx context.Context, id int64, account string, inactive bool, observedAt time.Time) (bool, error) {
	g := s.gate(id)
	g.write.Lock()
	defer g.write.Unlock()
	applied, err := s.Store.ObserveInstallation(ctx, id, account, inactive, observedAt)
	if err == nil && applied && inactive {
		g.stop()
	}
	return applied, err
}

// InstallationRemoved stops future writes and, for an uninstall, waits for
// the current run before deleting its records and review files.
func (s *Service) InstallationRemoved(ctx context.Context, id int64, deleted bool) error {
	return s.installationRemovedAt(ctx, id, deleted, time.Now())
}

func (s *Service) installationRemovedAt(ctx context.Context, id int64, deleted bool, observedAt time.Time) error {
	applied, err := s.observeInstallation(ctx, id, "", true, observedAt)
	if err != nil || !applied || !deleted {
		return err
	}
	return s.forgetIfInactiveBefore(context.WithoutCancel(ctx), id, observedAt)
}

// RepositoryAdded and RepositoryRemoved update access before the queued
// work can run. Removal cancels an in-flight run of that repository and
// prevents further writes from starting.
func (s *Service) RepositoryAdded(ctx context.Context, installation int64, repo string) error {
	g := s.gate(installation)
	g.write.Lock()
	defer g.write.Unlock()
	return s.Store.RepoAccess(ctx, installation, repo, true, time.Now())
}

func (s *Service) RepositoryRemoved(ctx context.Context, installation int64, repo string) error {
	g := s.gate(installation)
	g.write.Lock()
	err := s.Store.RepoAccess(ctx, installation, repo, false, time.Now())
	if err == nil {
		g.stopRepo(repo)
	}
	g.write.Unlock()
	return err
}

// SyncRepositoryWebhook checks current access rather than trusting a
// possibly delayed added or removed delivery. RepoAccess's observation
// time prevents a slower API read from undoing a newer one.
func (s *Service) SyncRepositoryWebhook(ctx context.Context, installation int64, repo string) error {
	observedAt := time.Now()
	repos, err := s.listRepos(ctx, installation)
	if err != nil {
		return fmt.Errorf("list installation %d repositories: %w", installation, err)
	}
	active := slices.ContainsFunc(repos, func(r string) bool { return strings.EqualFold(r, repo) })
	g := s.gate(installation)
	g.write.Lock()
	defer g.write.Unlock()
	if err := s.Store.RepoAccess(ctx, installation, repo, active, observedAt); err != nil {
		return err
	}
	inactive, err := s.Store.RepoInactive(ctx, installation, repo)
	if err != nil {
		return err
	}
	if inactive {
		g.stopRepo(repo)
	}
	return nil
}

// Base is the policy a repository gets before its own files: report
// mode, no Dependabot commands (Dependabot ignores an App's), and no
// agent review until the repository opts in.
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
	failures := 0
	for {
		if ids, err := s.SyncInstallations(ctx); err != nil {
			s.Log.Error("sweep", "err", err)
			failures++
		} else {
			failures = 0
			for _, id := range ids {
				enqueue(jobs.Job{Installation: id, Trigger: "schedule"})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepDelay(period, failures)):
		}
	}
}

// A failed sweep should retry soon, while repeated failures back off so
// an unavailable GitHub API is not polled continuously.
func sweepDelay(period time.Duration, failures int) time.Duration {
	if failures == 0 {
		return period
	}
	delays := [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}
	return min(period, delays[min(failures-1, len(delays)-1)])
}

// SyncInstallations records the installations GitHub lists as active,
// and returns them. One it no longer lists was uninstalled, its webhook
// perhaps missed: it is recorded as removed, and what was kept about its
// repositories is forgotten, as on the webhook. A suspended one is
// listed, and only stops running.
func (s *Service) SyncInstallations(ctx context.Context) ([]int64, error) {
	observedAt := time.Now()
	ins, err := s.Platform.Installations(ctx)
	if err != nil {
		return nil, err
	}
	var active []int64
	for _, in := range ins {
		if in.Suspended {
			if err := s.installationSuspendedAt(ctx, in.ID, in.Account, observedAt); err != nil {
				return nil, err
			}
			continue
		}
		if err := s.installationAddedAt(ctx, in.ID, in.Account, observedAt); err != nil {
			return nil, err
		}
		inactive, err := s.Store.InstallationInactive(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		if !inactive {
			active = append(active, in.ID)
		}
	}
	known, err := s.Store.Installations(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range known {
		listed := slices.ContainsFunc(ins, func(in app.Installation) bool { return in.ID == k.ID })
		if !listed {
			if err := s.installationRemovedAt(ctx, k.ID, true, observedAt); err != nil {
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
	type target struct {
		installation int64
		repo         string
	}
	var targets []target
	found := make(map[string]bool)
	for _, id := range ids {
		observedAt := time.Now()
		repos, err := s.listRepos(ctx, id)
		if err != nil {
			return err
		}
		if err := s.Store.ReconcileRepos(ctx, id, repos, observedAt); err != nil {
			return fmt.Errorf("reconcile installation %d repositories: %w", id, err)
		}
		for _, r := range repos {
			if len(only) > 0 && !slices.ContainsFunc(only, func(o string) bool { return strings.EqualFold(o, r) }) {
				continue
			}
			found[strings.ToLower(r)] = true
			if !s.runs(r) {
				if len(only) > 0 {
					return fmt.Errorf("repository %s is excluded by DMC_REPOS", r)
				}
				continue
			}
			targets = append(targets, target{id, r})
		}
	}
	for _, r := range only {
		if !found[strings.ToLower(r)] {
			return fmt.Errorf("repository %s is not accessible to this App; check that the App is installed on its owner and granted access to the repository", r)
		}
	}
	if len(targets) == 0 {
		return errors.New("no repositories available to run; check the App's installations and DMC_REPOS")
	}
	var failures []error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := s.RunRepo(ctx, t.installation, t.repo, "once"); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", t.repo, err))
		}
	}
	return errors.Join(append(failures, ctx.Err())...)
}

// Run does a queued job: one repository, or each of the installation's.
// Errors return to the queue so it can retry before the next sweep.
func (s *Service) Run(ctx context.Context, j jobs.Job) error {
	inactive, err := s.Store.InstallationInactive(ctx, j.Installation)
	if err != nil {
		return fmt.Errorf("check installation: %w", err)
	}
	if inactive {
		return nil
	}
	if j.Repo != "" {
		if s.runs(j.Repo) {
			if err := s.RunRepo(ctx, j.Installation, j.Repo, j.Trigger); err != nil {
				s.Log.Error("run repository", "repo", j.Repo, "err", err)
				return err
			}
		}
		return nil
	}
	observedAt := time.Now()
	repos, err := s.listRepos(ctx, j.Installation)
	if err != nil {
		s.Log.Error("list repositories", "installation", j.Installation, "err", err)
		return err
	}
	if err := s.Store.ReconcileRepos(ctx, j.Installation, repos, observedAt); err != nil {
		return fmt.Errorf("reconcile repositories: %w", err)
	}
	var failures []error
	for _, r := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.runs(r) {
			if err := s.RunRepo(ctx, j.Installation, r, j.Trigger); err != nil {
				s.Log.Error("run repository", "repo", r, "err", err)
				failures = append(failures, fmt.Errorf("%s: %w", r, err))
			}
		}
	}
	return errors.Join(failures...)
}

// RunRepo runs the engine on one repository and brings its issue up to
// date. The engine merges only when merging is allowed, the installation
// is not paused, and the repository's policy says mode: merge.
func (s *Service) RunRepo(ctx context.Context, installation int64, repo, trigger string) (runErr error) {
	g := s.gate(installation)
	g.run.Lock()
	defer g.run.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	g.start(repo, cancel)
	defer func() { g.finish(); cancel() }()
	inactive, err := s.Store.InstallationInactive(ctx, installation)
	if err != nil {
		return fmt.Errorf("check installation: %w", err)
	}
	if inactive {
		s.Log.Info("installation inactive; repository skipped", "installation", installation, "repo", repo)
		return nil
	}
	inactive, err = s.Store.RepoInactive(ctx, installation, repo)
	if err != nil {
		return fmt.Errorf("check repository access: %w", err)
	}
	if inactive {
		s.Log.Info("repository access removed; skipped", "installation", installation, "repo", repo)
		return nil
	}
	log := s.Log.With("installation", installation, "repo", repo, "trigger", trigger)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		log.Error("bad repository name")
		return errors.New("bad repository name")
	}
	started := time.Now()
	runID, err := s.Store.StartRun(ctx, installation, repo, trigger, started)
	if errors.Is(err, store.ErrInstallationInactive) {
		log.Info("installation inactive; repository skipped")
		return nil
	}
	if err != nil {
		log.Error("record run", "err", err)
		return err
	}
	outcome := store.RunOutcome{}
	defer func() {
		if ctx.Err() != nil && runErr == nil {
			runErr = ctx.Err()
		}
		if runErr != nil {
			outcome.Error = runErr.Error()
		}
		if err := s.Store.FinishRun(context.WithoutCancel(ctx), runID, outcome, time.Now()); err != nil {
			log.Error("record run", "err", err)
			runErr = errors.Join(runErr, fmt.Errorf("record run: %w", err))
		}
	}()

	if err := g.checkRateLimit(); err != nil {
		return err
	}
	paused, err := s.Store.Paused(ctx, installation)
	if err != nil {
		outcome.Error = err.Error()
		log.Error("run", "err", err)
		return err
	}
	merging := s.AllowMerge && !paused
	engine, err := s.Platform.Engine(ctx, installation)
	if err != nil {
		g.observeRateLimit(err)
		outcome.Error = err.Error()
		log.Error("run", "err", err)
		return err
	}
	client := &guarded{Client: engine, s: s, gate: g, installation: installation, repo: repo, runID: runID, log: log}
	r := runner.New(client, s.Platform.Login(), merging)
	r.Parallel = 1
	if s.Sleep != nil {
		r.Sleep = s.Sleep
	}
	var releaseReview func()
	defer func() {
		if releaseReview != nil {
			releaseReview()
		}
	}()
	res := r.RunRepoWithSetup(ctx, config.NewRepo(owner, name, Base()), func(rc config.Repo) error {
		if s.Review != nil && rc.Policy.AgentReview {
			// The in-memory installation gate covers only this Service.
			// Keep another server or -once process out of the checkout and
			// verdict cache until this run has finished using them.
			var err error
			releaseReview, err = s.lockReviewFiles(installation)
			if err != nil {
				return fmt.Errorf("agent review setup: %w", err)
			}
			// Another process may have completed uninstall cleanup after
			// our initial access check but before we acquired the file lock.
			// Do not recreate its checkouts or verdicts after deletion.
			inactive, err := s.Store.InstallationInactive(ctx, installation)
			if err != nil {
				return fmt.Errorf("agent review setup: check installation: %w", err)
			}
			if inactive {
				return fmt.Errorf("agent review setup: installation inactive")
			}
			inactive, err = s.Store.RepoInactive(ctx, installation, repo)
			if err != nil {
				return fmt.Errorf("agent review setup: check repository access: %w", err)
			}
			if inactive {
				return fmt.Errorf("agent review setup: repository access removed")
			}
			if err := s.setupReview(ctx, log, r, client, installation, repo); err != nil {
				return fmt.Errorf("agent review setup: %w", err)
			}
		}
		return nil
	})

	outcome.Error = res.Error
	if res.Failed() {
		if res.Error == "" {
			var failures []string
			for _, d := range res.Decisions {
				if d.Verdict != nil && d.Verdict.Error != "" {
					failures = append(failures, fmt.Sprintf("#%d: agent review failed: %s", d.Number, d.Verdict.Error))
				}
			}
			for _, ev := range res.Events {
				if ev.Kind == runner.EventMergeFailed || ev.Kind == runner.EventRebaseFailed {
					failures = append(failures, fmt.Sprintf("#%d: %s: %s", ev.Number, ev.Kind, ev.Detail))
				}
			}
			if len(failures) == 0 {
				failures = append(failures, "one or more PR actions or reviews failed")
			}
			outcome.Error = strings.Join(failures, "; ")
		}
		runErr = errors.New(outcome.Error)
	}
	if err := client.recordError(); err != nil {
		runErr = errors.Join(runErr, err)
	}
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
	recorded, err := s.track(context.WithoutCancel(ctx), log, client, runID, repo, res, client.seen, client.merged, repoMerges, complete)
	if err != nil {
		runErr = errors.Join(runErr, err)
	}
	if !recorded {
		// Another process completed cleanup while this run was reading.
		// Its results must neither restore records nor publish a stale report.
		return runErr
	}
	if runErr != nil {
		outcome.Error = runErr.Error()
	}
	if ctx.Err() != nil {
		return
	}
	if err := g.checkRateLimit(); err != nil {
		return errors.Join(runErr, err)
	}

	rest, err := s.Platform.REST(installation)
	if err != nil {
		log.Error("issue", "err", err)
		if outcome.Error == "" {
			outcome.Error = "issue: " + err.Error()
		}
		return errors.Join(runErr, fmt.Errorf("issue: %w", err))
	}
	rest, err = withRateLimit(rest, g)
	if err != nil {
		return errors.Join(runErr, fmt.Errorf("issue client: %w", err))
	}
	known, err := s.Store.IssueNumber(ctx, repo)
	if err != nil {
		log.Error("issue", "err", err)
		if outcome.Error == "" {
			outcome.Error = "issue: " + err.Error()
		}
		return errors.Join(runErr, fmt.Errorf("issue: %w", err))
	}
	// An uninstall or suspension can arrive while this run is reporting.
	// Hold the same gate used to mark removal across the issue write.
	g.write.Lock()
	inactive, stateErr := s.Store.InstallationInactive(ctx, installation)
	if stateErr != nil {
		g.write.Unlock()
		return errors.Join(runErr, fmt.Errorf("check installation: %w", stateErr))
	}
	if inactive {
		g.write.Unlock()
		return runErr
	}
	inactive, stateErr = s.Store.RepoInactive(ctx, installation, repo)
	if stateErr != nil {
		g.write.Unlock()
		return errors.Join(runErr, fmt.Errorf("check repository access: %w", stateErr))
	}
	if inactive {
		g.write.Unlock()
		return runErr
	}
	n, change, err := output.Sync(ctx, rest, s.Platform.Login(), owner, name, known, output.Body(res, repoMerges), output.Wanted(res), res.Error == "")
	g.write.Unlock()
	if n != known && n != 0 {
		if err := s.Store.SetRunIssueNumber(ctx, runID, n); err != nil {
			log.Error("record issue", "err", err)
			runErr = errors.Join(runErr, fmt.Errorf("record issue: %w", err))
			if outcome.Error == "" {
				outcome.Error = "record issue: " + err.Error()
			}
		}
	}
	if change != output.Unchanged {
		if err := s.record(ctx, log, store.Write{RunID: runID, Repo: repo, Kind: string(change), Number: n}); err != nil {
			runErr = errors.Join(runErr, err)
			outcome.Error = runErr.Error()
		}
	}
	if errors.Is(err, output.ErrIssuesDisabled) {
		log.Info("issues are disabled; nothing to report to")
		return runErr
	}
	if err != nil {
		log.Error("issue", "err", err)
		if outcome.Error == "" {
			outcome.Error = "issue: " + err.Error()
		}
		return errors.Join(runErr, fmt.Errorf("issue: %w", err))
	}
	log.Info("run done", "prs", outcome.PRs, "would_merge", outcome.WouldMerge, "merged", merged, "findings", outcome.Findings,
		"merging", repoMerges, "issue", n, "change", string(change), "error", res.Error, "took", time.Since(started).Round(time.Millisecond).String())
	return runErr
}

// track records the repository's mode and its Dependabot PRs as res saw
// them, for the test's numbers: each open PR, each the App merged, and
// how each PR open last time but not now left. The engine is asked about
// those, as merged or closed, and a merge the App's writes record is the
// App's. Only a complete run records the mode or judges which PRs left,
// and none when the PRs read were cut short (over 100). Evidence is saved
// atomically after the reads; recorded is false if cleanup deleted the run.
func (s *Service) track(ctx context.Context, log *slog.Logger, engine github.Client, runID int64, repo string, res runner.RepoResult, read map[int]github.PullRequest, mergedAt map[int]time.Time, merging, complete bool) (recorded bool, trackErr error) {
	now := time.Now()
	snapshot := store.RunSnapshot{Mode: res.Policy.Mode, Merging: merging, Complete: complete, At: now}
	defer func() {
		var err error
		recorded, err = s.Store.RecordRunSnapshot(ctx, runID, snapshot)
		if err != nil {
			log.Error("track run", "err", err)
			trackErr = errors.Join(trackErr, err)
		}
	}()
	var failures []error
	add := func(op string, err error) {
		log.Error(op, "err", err)
		failures = append(failures, fmt.Errorf("%s: %w", op, err))
	}
	before, err := s.Store.OpenPRs(ctx, repo)
	if err != nil {
		add("list tracked PRs", err)
		return false, errors.Join(failures...)
	}
	// Every PR the run read, those it merged part way included, was open
	// then; those in its last read are open still.
	for _, pr := range read {
		opened := pr.CreatedAt
		if opened.IsZero() {
			opened = now
		}
		snapshot.PRs = append(snapshot.PRs, store.PRObservation{Number: pr.Number, Title: pr.Title, OpenedAt: opened})
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
		snapshot.Closed = append(snapshot.Closed, store.PRClosure{Number: n, Outcome: store.PRMergedByApp, At: at})
	}
	if !complete {
		return false, errors.Join(failures...)
	}
	if slices.ContainsFunc(res.Findings, func(f policy.Finding) bool { return f.Kind == policy.FindingTruncated }) {
		return false, errors.Join(failures...)
	}
	owner, name, _ := strings.Cut(repo, "/")
	for _, n := range before {
		if _, merged := byApp[n]; seen[n] || merged {
			continue
		}
		state, err := engine.PullRequestState(ctx, owner, name, n)
		if err != nil {
			add(fmt.Sprintf("check PR #%d state", n), err)
			continue
		}
		outcome, at := store.PRClosed, now
		switch state {
		case github.PROpen:
			// No longer a Dependabot PR the search finds, yet open.
			continue
		case github.PRClosed:
		case github.PRMerged:
			outcome = store.PRMerged
			// Merged by a run that could not record it.
			if t, ok, err := s.Store.AppMerge(ctx, repo, n); err != nil {
				add(fmt.Sprintf("check PR #%d merge record", n), err)
				continue
			} else if ok {
				outcome, at = store.PRMergedByApp, t
			}
		default:
			add(fmt.Sprintf("check PR #%d state", n), fmt.Errorf("unrecognized pull request state %q", state))
			continue
		}
		snapshot.Closed = append(snapshot.Closed, store.PRClosure{Number: n, Outcome: outcome, At: at})
	}
	return false, errors.Join(failures...)
}

func (s *Service) record(ctx context.Context, log *slog.Logger, w store.Write) error {
	w.At = time.Now()
	if err := s.Store.RecordWrite(context.WithoutCancel(ctx), w); err != nil {
		log.Error("record write", "err", err, "kind", w.Kind, "number", w.Number)
		return fmt.Errorf("record %s #%d: %w", w.Kind, w.Number, err)
	}
	return nil
}

// ErrPaused is a write refused because the installation's writes are
// paused, or merging is not enabled.
var ErrPaused = errors.New("writes are paused for this installation")

// ErrNoComments is a comment the App refused to post: Dependabot ignores
// an App's commands, so only review explanations are allowed.
var ErrNoComments = errors.New("the App posts only agent review explanations, as Dependabot ignores an App's commands; set dependabot_commands: false")

// guarded is the engine's client for one run: every merge first checks
// that merging is still allowed, the kill switch included, so a pause
// stops a run part way, and is recorded with the head it was pinned to.
// Review comments check the same pause; Dependabot commands are refused.
type guarded struct {
	github.Client
	s            *Service
	gate         *installationGate
	installation int64
	repo         string
	runID        int64
	log          *slog.Logger

	mu sync.Mutex
	// seen are the Dependabot PRs the run read, by number.
	seen map[int]github.PullRequest
	// merged are when the run merged each PR it merged: a review can make
	// a run last long after.
	merged   map[int]time.Time
	auditErr error
}

func (g *guarded) record(ctx context.Context, w store.Write) {
	if err := g.s.record(ctx, g.log, w); err != nil {
		g.mu.Lock()
		g.auditErr = errors.Join(g.auditErr, err)
		g.mu.Unlock()
	}
}

func (g *guarded) recordError() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.auditErr
}

func (g *guarded) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	if err := g.gate.checkRateLimit(); err != nil {
		return nil, err
	}
	r, err := g.Client.FetchRepo(ctx, owner, name)
	g.gate.observeRateLimit(err)
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

func (g *guarded) FileText(ctx context.Context, owner, name, path string) ([]byte, error) {
	if err := g.gate.checkRateLimit(); err != nil {
		return nil, err
	}
	b, err := g.Client.FileText(ctx, owner, name, path)
	g.gate.observeRateLimit(err)
	return b, err
}

func (g *guarded) PullRequestDetails(ctx context.Context, owner, name string, number int) (*github.PullRequestDetails, error) {
	if err := g.gate.checkRateLimit(); err != nil {
		return nil, err
	}
	d, err := g.Client.PullRequestDetails(ctx, owner, name, number)
	g.gate.observeRateLimit(err)
	return d, err
}

func (g *guarded) PullRequestState(ctx context.Context, owner, name string, number int) (github.PRState, error) {
	if err := g.gate.checkRateLimit(); err != nil {
		return "", err
	}
	state, err := g.Client.PullRequestState(ctx, owner, name, number)
	g.gate.observeRateLimit(err)
	return state, err
}

func (g *guarded) Merge(ctx context.Context, owner, name string, number int, method, headOID string) error {
	g.gate.write.Lock()
	defer g.gate.write.Unlock()
	if err := g.gate.checkRateLimit(); err != nil {
		return err
	}
	inactive, err := g.s.Store.InstallationInactive(ctx, g.installation)
	if err != nil {
		return fmt.Errorf("checking the installation: %w", err)
	}
	if inactive {
		return ErrPaused
	}
	inactive, err = g.s.Store.RepoInactive(ctx, g.installation, g.repo)
	if err != nil {
		return fmt.Errorf("checking repository access: %w", err)
	}
	if inactive {
		return ErrPaused
	}
	paused, err := g.s.Store.Paused(ctx, g.installation)
	switch {
	case err != nil:
		return fmt.Errorf("checking the pause: %w", err)
	case paused || !g.s.AllowMerge:
		g.record(ctx, store.Write{RunID: g.runID, Repo: g.repo, Kind: "merge-refused", Number: number, Detail: "paused"})
		return ErrPaused
	}
	err = g.Client.Merge(ctx, owner, name, number, method, headOID)
	g.gate.observeRateLimit(err)
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
	g.record(ctx, w)
	return err
}

func (g *guarded) Comment(ctx context.Context, owner, name string, number int, body string) error {
	if !strings.HasPrefix(body, runner.ReviewCommentPrefix) {
		return ErrNoComments
	}
	g.gate.write.Lock()
	defer g.gate.write.Unlock()
	if err := g.gate.checkRateLimit(); err != nil {
		return err
	}
	inactive, err := g.s.Store.InstallationInactive(ctx, g.installation)
	if err != nil {
		return fmt.Errorf("checking the installation: %w", err)
	}
	if inactive {
		return ErrPaused
	}
	inactive, err = g.s.Store.RepoInactive(ctx, g.installation, g.repo)
	if err != nil {
		return fmt.Errorf("checking repository access: %w", err)
	}
	if inactive {
		return ErrPaused
	}
	paused, err := g.s.Store.Paused(ctx, g.installation)
	switch {
	case err != nil:
		return fmt.Errorf("checking the pause: %w", err)
	case paused || !g.s.AllowMerge:
		g.record(ctx, store.Write{RunID: g.runID, Repo: g.repo, Kind: "review-comment-refused", Number: number, Detail: "paused"})
		return ErrPaused
	}
	err = g.Client.Comment(ctx, owner, name, number, body)
	g.gate.observeRateLimit(err)
	w := store.Write{RunID: g.runID, Repo: g.repo, Kind: "review-comment", Number: number}
	if err != nil {
		w.Kind, w.Detail = "review-comment-attempted", err.Error()
	}
	g.record(ctx, w)
	return err
}
