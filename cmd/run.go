package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/cli/go-gh/v2/pkg/term"
	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/report"
	"github.com/sglavoie/deps-merger/internal/review"
	"github.com/sglavoie/deps-merger/internal/runner"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var (
		apply      bool
		asJSON     bool
		repos      []string
		reviews    bool
		agentName  string
		maxReviews int
		open       bool
	)
	c := &cobra.Command{
		Use:   "run",
		Short: "Evaluate Dependabot PRs and, with --apply, merge the safe ones",
		Long: `Evaluate every open Dependabot PR in the configured repositories.

Without --apply this is a dry run: it reports what would be merged or
rebased and what needs fixing, and changes nothing. With --apply it merges
safe PRs one at a time per repository and comments "@dependabot rebase" on
safe PRs that are behind their base branch. Up to four repositories are
processed at once; the report keeps the config order.

With --review (or --agent), an AI agent judges the PRs the policy alone
would not merge because of their size: updates over max_update_type and
updates of unknown size, once everything else about them is safe. The
agent only reads a checkout of the PR; an update it approves is merged
like any other, after every check has been made again. Verdicts are
cached, so a scheduled run does not pay twice for the same update. A dry
run reviews too, and shows what would be merged.

With --open, repositories without open Dependabot PRs are left out of the
report; ones that could not be processed are still shown.

Exits 1 if a repository could not be processed or a merge or rebase request
failed or an agent review could not be done, and 130 when interrupted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			selected, err := cfg.Filter(repos)
			if err != nil {
				return err
			}
			if len(selected) == 0 {
				return errors.New("every configured repo has skip: true; name one with --repo to run it")
			}
			wantReviews := reviews || agentName != ""
			if cmd.Flags().Changed("max-reviews") && !wantReviews {
				return errors.New("--max-reviews needs --review or --agent")
			}
			if maxReviews < 0 {
				return errors.New("--max-reviews must not be negative")
			}
			var agent review.Agent
			switch {
			case wantReviews && !slices.ContainsFunc(selected, func(r config.Repo) bool { return r.Policy.AgentReview }):
				// No repo would use the agent, so neither require it
				// installed nor take the review lock.
				if agentName != "" && !config.ValidAgent(strings.ToLower(agentName)) {
					return fmt.Errorf("unknown agent %q (want %s)", agentName, strings.Join(config.Agents, " or "))
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "note: no agent reviews: every selected repo sets agent_review: false")
			case wantReviews:
				if agent, err = newAgent(cfg.Agent, agentName); err != nil {
					return err
				}
			}
			// SIGHUP too: an agent runs in a process group of its own, so
			// the hangup of the terminal would kill deps-merger and leave
			// the agent running rather than kill it.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			// The first signal starts an orderly stop; once it has, the
			// signals act as they normally do, so a second one kills the
			// process should that stop hang.
			context.AfterFunc(ctx, stop)

			client, err := newClient()
			if err != nil {
				return err
			}
			viewer, err := github.Again(ctx, 3*time.Second, nil, func() (string, error) { return client.Viewer(ctx) })
			if err != nil {
				if ctx.Err() != nil {
					return errInterrupted
				}
				return err
			}

			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			var progress func(runner.RepoResult)
			if !asJSON {
				progress = func(r runner.RepoResult) {
					if !open || report.HasPRs(r) {
						report.Repo(out, apply, r)
					}
				}
			} else if f, ok := errOut.(*os.File); ok && term.IsTerminal(f) {
				// JSON comes at the end; say what is happening meanwhile,
				// unless stderr is captured along with it.
				progress = func(r runner.RepoResult) { report.Progress(errOut, r) }
			}
			r := runner.New(client, viewer, apply)
			if agent != nil {
				release, err := setupReviews(r, cfg, agent, client, maxReviews)
				if err != nil {
					return err
				}
				defer release()
				if msg := r.Verdicts.TakeRecovered(); msg != "" {
					r.Warn(msg)
				}
			}
			results := r.Run(ctx, selected, progress)
			shown := results
			if open {
				shown = slices.DeleteFunc(slices.Clone(results), func(r runner.RepoResult) bool { return !report.HasPRs(r) })
			}
			if asJSON {
				if err := report.JSON(out, apply, shown); err != nil {
					return err
				}
			} else {
				report.Summary(out, apply, shown, len(results)-len(shown))
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			for _, r := range results {
				if r.Failed() {
					return exitError{code: 1}
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "merge safe PRs and request rebases (default is a dry run)")
	c.Flags().BoolVar(&asJSON, "json", false, "print results as JSON")
	c.Flags().StringSliceVarP(&repos, "repo", "r", nil, "only handle these repos (owner/name or name; repeatable)")
	c.Flags().BoolVar(&reviews, "review", false, "let an AI agent judge major updates and updates of unknown size")
	c.Flags().StringVar(&agentName, "agent", "", "agent to review with: "+strings.Join(config.Agents, " or ")+" (default from config; implies --review)")
	c.Flags().BoolVar(&open, "open", false, "only report repos with open Dependabot PRs")
	c.Flags().IntVar(&maxReviews, "max-reviews", 10, "most agent reviews in one run (0 for no limit)")
	return c
}

// newAgent builds the agent named by --agent, or the configured one.
func newAgent(cfg config.Agent, name string) (review.Agent, error) {
	if name == "" {
		name = cfg.Use
	}
	var agent review.Agent
	switch strings.ToLower(name) {
	case "claude":
		agent = review.Claude{Model: cfg.Claude.Model, ConfigDir: cfg.Claude.ConfigDir, MaxBudgetUSD: cfg.Claude.MaxBudgetUSD}
	case "codex":
		agent = review.Codex{Model: cfg.Codex.Model, Home: cfg.Codex.Home}
	default:
		return nil, fmt.Errorf("unknown agent %q (want %s)", name, strings.Join(config.Agents, " or "))
	}
	if _, err := exec.LookPath(agent.Name()); err != nil {
		return nil, fmt.Errorf("%s is not installed or not on PATH", agent.Name())
	}
	return agent, nil
}

// newClient builds the GitHub client; tests replace it.
var newClient = github.New

// setupReviews gives r an agent reviewer with its checkouts and verdict
// cache in the XDG cache and state directories. It holds a lock until
// release is called, so a second review run cannot share the checkouts,
// and removes the checkouts of repositories not in the config that no
// review used for 30 days. A checkout it cannot remove is a warning of
// the run's, not a reason to skip the reviews.
func setupReviews(r *runner.Runner, cfg *config.Config, agent review.Agent, client github.Client, maxReviews int) (release func(), err error) {
	cacheDir, err := config.CacheDir()
	if err != nil {
		return nil, err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	if release, err = review.Lock(filepath.Join(stateDir, "review.lock")); err != nil {
		return nil, err
	}
	verdicts, err := review.OpenCache(verdictsPath(stateDir))
	if err != nil {
		release()
		return nil, err
	}
	token, _ := auth.TokenForHost("github.com")
	ws := &review.Workspace{Root: filepath.Join(cacheDir, "checkouts"), Token: token}
	keep := map[string]bool{}
	for _, rc := range cfg.Repos {
		keep[rc.FullName()] = true
	}
	if err := ws.Prune(keep); err != nil {
		r.Warn("prune checkouts: " + err.Error())
	}
	r.Reviewer = &review.AgentReviewer{
		Agent:     agent,
		Details:   client,
		Workspace: ws,
		Timeout:   cfg.Agent.Timeout,
	}
	r.AgentName, r.Verdicts, r.MaxReviews = agent.Name(), verdicts, maxReviews
	return release, nil
}

// verdictsPath is where agent verdicts are kept between runs.
func verdictsPath(stateDir string) string { return filepath.Join(stateDir, "verdicts.json") }

var errInterrupted = errors.New("interrupted")
