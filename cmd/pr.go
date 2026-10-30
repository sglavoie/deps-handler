package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/pr"
	"github.com/sglavoie/deps-merger/pkg/review"
	"github.com/spf13/cobra"
)

var prCommandsAvailable = true

var newPRClient = github.NewPRClient
var newPRWorkspace = func(root string) pr.Workspace {
	token, _ := auth.TokenForHost("github.com")
	return &review.Workspace{Root: root, Token: token}
}
var newLocalPRWorkspace = func(ctx context.Context, directory string) (pr.Workspace, func(), error) {
	token, _ := auth.TokenForHost("github.com")
	workspace, err := review.NewLocalWorkspace(ctx, directory, token)
	if err != nil {
		return nil, nil, err
	}
	return workspace, workspace.Close, nil
}
var newTaskAgent = func(cfg config.Agent, name string) (review.TaskAgent, error) {
	a, err := newAgent(cfg, name)
	if err != nil {
		return nil, err
	}
	task, ok := a.(review.TaskAgent)
	if !ok {
		return nil, errors.New("agent does not support PR tasks")
	}
	return task, nil
}

func newPRCmd() *cobra.Command {
	c := &cobra.Command{Use: "pr", Short: "Diagnose or repair one explicitly selected pull request", Args: cobra.NoArgs}
	c.AddCommand(newPRTaskCmd(false), newPRTaskCmd(true))
	return c
}

func newPRTaskCmd(fix bool) *cobra.Command {
	var agentName, focus, planPath, workingDirectory string
	var asJSON bool
	var attempts int
	var waitTimeout time.Duration
	verb, short := "diagnose", "Explain merge blockers and produce a reusable AI repair handoff"
	if fix {
		verb, short = "fix", "Edit, commit and push AI repairs, then verify the PR's CI"
	}
	targetArgs := cobra.ExactArgs(1)
	use := verb + " <owner/repo#number | PR-URL>"
	if fix {
		targetArgs = cobra.MaximumNArgs(1)
		use = verb + " [owner/repo#number | PR-URL]"
	}
	c := &cobra.Command{
		Use: use, Short: short,
		Long: prTaskHelp(fix),
		Args: targetArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var plan *pr.RepairPlan
			if fix && cmd.Flags().Changed("plan") {
				if strings.TrimSpace(planPath) == "" {
					return usageError{errors.New("--plan needs a Markdown file path")}
				}
				var err error
				plan, err = pr.LoadPlan(planPath)
				if err != nil {
					return err
				}
			}
			var target github.PRTarget
			if len(args) == 1 {
				var err error
				target, err = github.ParsePRTarget(args[0])
				if err != nil {
					return usageError{err}
				}
			} else if plan != nil && plan.Target.Number > 0 {
				target = plan.Target
			} else {
				return usageError{errors.New("provide a PR target or --plan with PR metadata from pr diagnose")}
			}
			if plan != nil {
				if err := plan.ValidateTarget(target); err != nil {
					return usageError{err}
				}
			}
			if cmd.Flags().Changed("working-directory") && strings.TrimSpace(workingDirectory) == "" {
				return usageError{errors.New("--working-directory needs a directory path")}
			}
			if cmd.Flags().Changed("agent") && strings.TrimSpace(agentName) == "" {
				return usageError{errors.New("--agent needs an agent name")}
			}
			name := strings.ToLower(agentName)
			if name != "" && !config.ValidAgent(name) {
				return usageError{fmt.Errorf("unknown agent %q (want %s)", agentName, config.AgentList())}
			}
			if fix && (attempts < 1 || waitTimeout <= 0) {
				return usageError{errors.New("--max-attempts and --wait-timeout must be positive")}
			}
			cfg, err := loadPRConfig()
			if err != nil {
				return err
			}
			if name == "" {
				name = cfg.Use
			}
			if fix && name == "api" {
				return usageError{errors.New("pr fix needs --agent claude or --agent codex; api supports pr diagnose only")}
			}
			// The demo build must remain unable to reach real repositories.
			if !prCommandsAvailable {
				return errors.New("pr commands are unavailable in the demo build")
			}
			agent, err := newTaskAgent(cfg, name)
			if err != nil {
				return err
			}
			client, err := newPRClient()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			context.AfterFunc(ctx, stop)
			var workspace pr.Workspace
			root := ""
			local := workingDirectory != ""
			keep := local
			if local {
				var release func()
				workspace, release, err = newLocalPRWorkspace(ctx, workingDirectory)
				if err != nil {
					return err
				}
				defer release()
			} else {
				root, err = os.MkdirTemp("", "deps-merger-pr-")
				if err != nil {
					return err
				}
				workspace = newPRWorkspace(filepath.Join(root, "checkout"))
			}
			defer func() {
				if root != "" && !keep {
					_ = os.RemoveAll(root)
				}
			}()
			service := &pr.Service{Client: client, Agent: agent, Plan: plan, Workspace: workspace,
				AgentTimeout: cfg.Timeout, WaitTimeout: waitTimeout, PollInterval: 10 * time.Second, MaxAttempts: attempts,
				Progress: func(message string) { fmt.Fprintln(cmd.ErrOrStderr(), review.Clean(message)) },
			}
			var result pr.Result
			if fix {
				result, err = service.Fix(ctx, target, focus)
			} else {
				result, err = service.Diagnose(ctx, target, focus)
			}
			if err != nil {
				result.Error = review.Clean(err.Error())
			}
			keep = local || (fix && err != nil && result.Workspace != "")
			if !keep {
				result.Workspace = ""
			} else if result.Workspace != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "Repair checkout kept at", result.Workspace)
			}
			if asJSON {
				data, marshalErr := json.MarshalIndent(result, "", "  ")
				if marshalErr != nil {
					return marshalErr
				}
				if _, writeErr := fmt.Fprintln(cmd.OutOrStdout(), review.Clean(string(data))); writeErr != nil {
					return writeErr
				}
			} else if result.Snapshot.HeadOID != "" {
				if writeErr := pr.Markdown(cmd.OutOrStdout(), result); writeErr != nil {
					return writeErr
				}
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			if err != nil {
				return errors.New(review.Clean(err.Error()))
			}
			return nil
		},
	}
	c.Flags().StringVar(&agentName, "agent", "", "agent: "+config.AgentList()+" (default from config, otherwise claude)")
	c.Flags().StringVar(&focus, "focus", "", "failure or area to investigate")
	c.Flags().BoolVar(&asJSON, "json", false, "print the structured report as JSON")
	if fix {
		c.Flags().StringVar(&workingDirectory, "working-directory", "", "edit this existing clean checkout at the PR head; preserve it and its installed dependencies")
		_ = c.MarkFlagDirname("working-directory")
		c.Flags().StringVar(&planPath, "plan", "", "follow a saved Markdown repair plan (infer PR from diagnose report when omitted)")
		_ = c.MarkFlagFilename("plan", "md", "markdown")
		c.Flags().IntVar(&attempts, "max-attempts", 3, "maximum edit/push/CI attempts")
		c.Flags().DurationVar(&waitTimeout, "wait-timeout", 15*time.Minute, "maximum wait for CI per commit")
	}
	return c
}

func loadPRConfig() (config.Agent, error) {
	path := configPath
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return config.Agent{}, err
		}
	}
	cfg, err := config.LoadForPR(path)
	if configPath == "" && errors.Is(err, os.ErrNotExist) {
		return config.Agent{Use: config.Agents[0], Timeout: config.DefaultAgentTimeout}, nil
	}
	if err != nil {
		return config.Agent{}, err
	}
	return cfg.Agent, nil
}

func prTaskHelp(fix bool) string {
	common := `
Accepts a github.com PR URL or owner/repo#number, for any PR author.
Agent settings come from your config when present; a configured repository
or config file is not required. Without a config, Claude is the default.
--agent overrides that choice. --focus selects a failure or area to examine.
--json prints a structured report. Invalid arguments exit 2; interrupts exit 130.`
	if fix {
		return `Repair the selected PR in a private disposable checkout by default. This command
explicitly authorizes the AI to edit files, run tests and push commits to the
PR head branch. It checks for concurrent updates, waits for CI and retries
failures up to --max-attempts. --wait-timeout bounds each wait for CI.

Use --plan repair.md to implement a saved diagnosis or edited repair handoff.
The PR can be omitted when the file contains metadata from pr diagnose. An
explicit PR must match the report. The saved steps guide the repair; stale
plans and later retries get fresh evidence to adapt to current code.

Use --working-directory /path/to/repo to edit an existing local checkout.
It must be clean and already checked out at the selected PR head, on a
non-base branch or detached HEAD. The command never resets or cleans it;
ignored dependencies are reused, and the directory is kept on every outcome.

Requires Claude or Codex. It never merges the PR; reviews and other manual
merge gates remain in the report. A failed repair keeps its checkout for
recovery. Exit 0 confirms passing CI, with code merge conflicts resolved;
exit 1 means repair or CI confirmation failed.
` + common
	}
	return `Read the selected PR and produce a concrete AI repair handoff. The report
includes current checks, merge blockers, failure evidence, proposed edits
and validation commands. It works for passing, failing or pending CI.

Prints Markdown you can save or paste into a coding session. The checkout
is read-only to the agent and nothing is pushed. Supports Claude, Codex or
the API agent. A completed report exits 0 even when it finds blockers;
an operational failure exits 1.
` + common
}
