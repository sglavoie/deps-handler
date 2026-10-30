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
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/report"
	"github.com/sglavoie/deps-handler/pkg/review"
	"github.com/sglavoie/deps-handler/pkg/runner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func newDependabotCmd() *cobra.Command {
	var (
		apply      bool
		asJSON     bool
		markdown   bool
		repos      []string
		owners     []string
		allRepos   bool
		reviews    bool
		agentName  string
		maxReviews int
		open       bool
		parallel   int
		reviewPar  int
	)
	c := &cobra.Command{
		Use:   "dependabot",
		Short: "Evaluate Dependabot PRs and, with --apply, merge the safe ones",
		Long: `Evaluate every open Dependabot PR in the configured repositories.

Without --apply this is a dry run: it reports what would be merged or
rebased and what needs fixing, and changes nothing. With --apply it merges
safe PRs one at a time per repository and comments "@dependabot rebase" on
safe PRs that are behind their base branch. Up to --parallel repositories
are processed at once; the report keeps the config order.

With --review (or --agent), an AI agent judges the PRs the policy alone
would not merge because of their size: updates over max_update_type and
updates of unknown size, once everything else about them is safe. The
agent only reads a checkout of the PR; an update it approves is merged
like any other, after every check has been made again. Verdicts are
cached, so a scheduled run does not pay twice for the same update. A dry
run reviews too, and shows what would be merged. Up to --review-parallel
agents run at once.

With --repo and --owner, only the named repositories and the repositories
of the named owners run; --owner leaves out repositories set to skip: true,
which --repo runs when named. --include-skipped runs those as well, alone
or with --owner.

With --open, repositories without open Dependabot PRs are left out of the
report; ones that could not be processed are still shown.

With --markdown, each repository's section is printed as GitHub Markdown,
as an issue body would hold it, and no summary follows.

Exits 1 if a repository could not be processed, a merge or rebase request
failed, an agent review could not be done or the report could not be written;
2 when the command line is
wrong, including a --repo the config does not list or an --owner it has
no repository of; and 130 when interrupted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Flags first: a mistake in the command line is reported
			// before anything in the config.
			// An empty --agent, as an unset variable gives, must not turn
			// reviews off.
			if cmd.Flags().Changed("agent") && strings.TrimSpace(agentName) == "" {
				return usageError{errors.New("--agent needs an agent name")}
			}
			wantReviews := reviews || agentName != ""
			if asJSON && markdown {
				return usageError{errors.New("--json and --markdown cannot be used together")}
			}
			if cmd.Flags().Changed("max-reviews") && !wantReviews {
				return usageError{errors.New("--max-reviews needs --review or --agent")}
			}
			if maxReviews < 0 {
				return usageError{errors.New("--max-reviews must not be negative")}
			}
			if cmd.Flags().Changed("review-parallel") && !wantReviews {
				return usageError{errors.New("--review-parallel needs --review or --agent")}
			}
			if parallel < 1 {
				return usageError{errors.New("--parallel must be at least 1")}
			}
			if reviewPar < 1 {
				return usageError{errors.New("--review-parallel must be at least 1")}
			}
			if agentName != "" && !config.ValidAgent(strings.ToLower(agentName)) {
				return usageError{fmt.Errorf("unknown agent %q (want %s)", agentName, config.AgentList())}
			}
			if err := checkSelection(cmd, repos, owners); err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			selected, err := cfg.Filter(repos, owners, allRepos)
			if err != nil {
				// A repo the config lacks is a mistake of the command
				// line, not of the config.
				return usageError{err}
			}
			if len(selected) == 0 {
				if len(owners) > 0 {
					return errors.New("every repo of the named owners has skip: true; name one with --repo or add --include-skipped to run them")
				}
				return errors.New("every configured repo has skip: true; name one with --repo or add --include-skipped to run them")
			}
			var agent review.Agent
			switch {
			case wantReviews && !slices.ContainsFunc(selected, config.Repo.MayReview):
				// No repo would use the agent, so neither require it
				// installed nor take the review lock. A repo whose
				// agent_review comes from the defaults is not one: its
				// policy files may turn reviews on.
				fmt.Fprintln(cmd.ErrOrStderr(), "note: no agent reviews: every selected repo sets agent_review: false")
			case wantReviews:
				if agent, err = newAgent(cfg.Agent, agentName); err != nil {
					return err
				}
			}
			// SIGHUP too: an agent runs in a process group of its own, so
			// the hangup of the terminal would kill deps-handler and leave
			// the agent running rather than kill it.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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
			var outputErr error
			var progress func(runner.RepoResult)
			if markdown {
				progress = func(r runner.RepoResult) {
					if outputErr == nil && (!open || report.HasPRs(r)) {
						outputErr = report.Markdown(out, apply, r)
					}
				}
			} else if !asJSON {
				progress = func(r runner.RepoResult) {
					if outputErr == nil && (!open || report.HasPRs(r)) {
						outputErr = report.Repo(out, apply, r)
					}
				}
			} else if f, ok := errOut.(*os.File); ok && term.IsTerminal(f) {
				// JSON comes at the end; say what is happening meanwhile,
				// unless stderr is captured along with it.
				progress = func(r runner.RepoResult) { report.Progress(errOut, r) }
			}
			r := runner.New(client, viewer, apply)
			r.Parallel, r.ReviewParallel = parallel, reviewPar
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
			if outputErr != nil {
				return outputErr
			}
			shown := results
			if open {
				shown = slices.DeleteFunc(slices.Clone(results), func(r runner.RepoResult) bool { return !report.HasPRs(r) })
			}
			switch {
			case asJSON:
				if err := report.JSON(out, apply, shown); err != nil {
					return err
				}
			case !markdown:
				if err := report.Summary(out, apply, shown, len(results)-len(shown)); err != nil {
					return err
				}
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
	c.Flags().BoolVar(&markdown, "markdown", false, "print each repository's section as GitHub Markdown")
	c.Flags().StringSliceVarP(&repos, "repo", "r", nil, "only handle these repos (owner/name or name; repeatable)")
	c.Flags().StringSliceVar(&owners, "owner", nil, "only handle the configured repos of these owners (repeatable)")
	keepEmpty(c, "repo", "owner")
	c.Flags().BoolVar(&allRepos, "include-skipped", false, "also handle repos set to skip: true")
	c.Flags().BoolVar(&reviews, "review", false, "let an AI agent judge updates over max_update_type and of unknown size")
	c.Flags().StringVar(&agentName, "agent", "", "agent to review with: "+config.AgentList()+" (default from config; implies --review)")
	c.Flags().BoolVar(&open, "open", false, "only report repos with open Dependabot PRs")
	c.Flags().IntVar(&maxReviews, "max-reviews", 10, "most agent reviews in one run (0 for no limit)")
	c.Flags().IntVar(&parallel, "parallel", runner.DefaultParallel, "most repositories processed at once")
	c.Flags().IntVar(&reviewPar, "review-parallel", runner.DefaultReviewParallel, "most agent reviews run at once")
	completeSelection(c)
	return c
}

// checkSelection trims the names given to --repo and --owner in place and
// rejects empty ones, and owners written owner/name, as usage errors.
func checkSelection(cmd *cobra.Command, repos, owners []string) error {
	// An empty --repo, as an unset variable in a script gives, must not
	// mean every repo, as no --repo at all does.
	if cmd.Flags().Changed("repo") {
		if len(repos) == 0 {
			return usageError{errors.New("--repo needs a repo name")}
		}
		if gaveEmpty(cmd, "repo") {
			return usageError{errors.New("--repo has an empty repo name")}
		}
		for i := range repos {
			if repos[i] = strings.TrimSpace(repos[i]); repos[i] == "" {
				return usageError{errors.New("--repo has an empty repo name")}
			}
		}
	}
	if cmd.Flags().Changed("owner") {
		if len(owners) == 0 {
			return usageError{errors.New("--owner needs an owner name")}
		}
		if gaveEmpty(cmd, "owner") {
			return usageError{errors.New("--owner has an empty owner name")}
		}
		for i := range owners {
			switch owners[i] = strings.TrimSpace(owners[i]); {
			case owners[i] == "":
				return usageError{errors.New("--owner has an empty owner name")}
			case strings.Contains(owners[i], "/"):
				return usageError{fmt.Errorf("--owner takes an owner, not %q; use --repo for one repo", owners[i])}
			}
		}
	}
	return nil
}

// emptyName is a --repo or --owner flag that remembers being given an
// empty value: pflag's string slice drops one given beside others, as in
// -r "" -r a, which would then mean the other repos alone.
type emptyName struct {
	pflag.Value
	given bool
}

func (e *emptyName) Set(s string) error {
	e.given = e.given || strings.TrimSpace(s) == ""
	return e.Value.Set(s)
}

// keepEmpty has c's flags of these names remember an empty value, for
// checkSelection to reject.
func keepEmpty(c *cobra.Command, names ...string) {
	for _, name := range names {
		f := c.Flags().Lookup(name)
		f.Value = &emptyName{Value: f.Value}
		// Help would otherwise show "(default [])", not knowing the type.
		f.DefValue = ""
	}
}

func gaveEmpty(cmd *cobra.Command, name string) bool {
	e, ok := cmd.Flags().Lookup(name).Value.(*emptyName)
	return ok && e.given
}

// newAgent builds the agent named by --agent, or the configured one; the
// demo build replaces it.
var newAgent = func(cfg config.Agent, name string) (review.Agent, error) {
	if name == "" {
		name = cfg.Use
	}
	var agent review.Agent
	switch strings.ToLower(name) {
	case "claude":
		agent = review.Claude{Model: cfg.Claude.Model, ConfigDir: cfg.Claude.ConfigDir, MaxBudgetUSD: cfg.Claude.MaxBudgetUSD}
	case "codex":
		agent = review.Codex{Model: cfg.Codex.Model, Home: cfg.Codex.Home}
	case "api":
		return newAPIAgent(cfg.API)
	default:
		return nil, fmt.Errorf("unknown agent %q (want %s)", name, config.AgentList())
	}
	if _, err := exec.LookPath(agent.Name()); err != nil {
		return nil, fmt.Errorf("%s is not installed or not on PATH", agent.Name())
	}
	return agent, nil
}

// newAPIAgent builds the api agent, reading its key from the variable
// key_env names. It runs no program, so nothing is looked up on PATH.
func newAPIAgent(cfg config.API) (review.Agent, error) {
	if cfg.URL == "" || cfg.Model == "" {
		return nil, errors.New("the api agent needs api.url and api.model in the config")
	}
	key := ""
	if cfg.KeyEnv != "" {
		if key = os.Getenv(cfg.KeyEnv); key == "" {
			return nil, fmt.Errorf("api.key_env names %s, which is not set", cfg.KeyEnv)
		}
	}
	return review.API{URL: cfg.URL, Model: cfg.Model, Key: key, Tools: cfg.UsesTools(), MaxTurns: cfg.MaxTurns}, nil
}

// newClient builds the GitHub client; tests and the demo build replace it.
var newClient = github.New

// setupReviews gives r an agent reviewer with its checkouts and verdict
// cache in the XDG cache and state directories. It holds locks until
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
	// Checkouts live under the cache directory, which can be shared by
	// runs using different state directories. Keep the state lock too,
	// for compatibility with runs started by earlier versions.
	releaseCheckouts, err := review.Lock(filepath.Join(cacheDir, "checkouts.lock"))
	if err != nil {
		release()
		return nil, err
	}
	releaseState := release
	release = func() {
		releaseCheckouts()
		releaseState()
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
	r.Reviewer = newReviewer(agent, client, ws, cfg.Agent.Timeout)
	r.AgentName, r.Verdicts, r.MaxReviews = agent.Name(), verdicts, maxReviews
	return release, nil
}

// newReviewer has agent review PRs in checkouts under ws; the demo build
// replaces it.
var newReviewer = func(agent review.Agent, client github.Client, ws *review.Workspace, timeout time.Duration) review.Reviewer {
	return &review.AgentReviewer{Agent: agent, Details: client, Workspace: ws, Timeout: timeout}
}

// verdictsPath is where agent verdicts are kept between runs.
func verdictsPath(stateDir string) string { return filepath.Join(stateDir, "verdicts.json") }

var errInterrupted = errors.New("interrupted")

// completeSelection completes --repo and --owner from the config, so a
// shell can offer the configured repositories and their owners.
func completeSelection(c *cobra.Command) {
	_ = c.RegisterFlagCompletionFunc("repo", completeFromConfig(func(r config.Repo) string { return r.FullName() }))
	_ = c.RegisterFlagCompletionFunc("owner", completeFromConfig(func(r config.Repo) string { return r.Owner }))
}

// completeFromConfig completes with what name gives for each configured
// repository, each once and in config order.
func completeFromConfig(name func(config.Repo) string) cobra.CompletionFunc {
	return func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		cfg, err := loadConfig()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		var out []cobra.Completion
		for _, r := range cfg.Repos {
			if n := name(r); !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}
