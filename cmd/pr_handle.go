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

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
	"github.com/spf13/cobra"
)

var newPRHandleClient = github.NewHandleClient

// Delay agent setup until an eligible PR actually needs a review. Empty
// repositories and excluded PRs should not require an installed coding agent.
type handleTaskAgent struct {
	cfg   config.Agent
	name  string
	agent review.TaskAgent
}

func (a *handleTaskAgent) Name() string { return a.name }

func (a *handleTaskAgent) Task(ctx context.Context, dir, prompt, schema string, writable bool) (json.RawMessage, error) {
	if a.agent == nil {
		var err error
		a.agent, err = newTaskAgent(a.cfg, a.name)
		if err != nil {
			return nil, err
		}
	}
	return a.agent.Task(ctx, dir, prompt, schema, writable)
}

func newPRHandleCmd() *cobra.Command {
	var repository, agentName string
	var opts pr.HandleOptions
	var attempts int
	var waitTimeout time.Duration
	var output *outputFormat
	c := &cobra.Command{
		Use:   "handle [owner/repo#number | PR-URL ...]",
		Short: "Review non-Dependabot PRs; --apply repairs, verifies CI and merges",
		Long: `Review exactly the named PRs, or all open PRs in --repo owner/name.
Dependabot-authored PRs are always excluded, even when explicitly named or
later edited by someone else. Drafts and closed PRs are skipped. Other bots
are eligible. No configured repository list is required or selected implicitly.

Without --apply, inspect code, CI and discussions and report proposed actions.
With --apply, repair supported code problems, push repair commits, rerun
identified transient Actions failures and merge approved PRs when CI and
GitHub's other merge requirements pass. AI review also runs on green PRs;
every repair receives a fresh review. AI verdicts are not GitHub approvals.
Human decisions and account actions remain blockers. No reviews or comments
are posted, discussions are not resolved, and drafts are never marked ready.

Repository discovery completes before processing starts, oldest PR first.
PRs are handled serially with fresh state after each merge; blocked PRs do
not prevent processing the rest. New PRs wait for the next invocation.
Concurrent code, base or discussion changes invalidate the current review.
Repairs and reruns share --max-attempts per PR, followed by a final review.
Each workflow can be rerun at most once per head. Missing or incomplete CI
and discussions block merging. A failed apply retains its private checkout.

Merge method defaults to squash, then merge or rebase if squash is disabled.
--merge-method explicitly requires that method. Agent settings come from
config when present; Claude is the default. Applying requires Claude or Codex;
the API agent supports preview only. --wait-timeout bounds each CI wait.

Exit 0 means the preview completed, or all eligible PRs merged with --apply.
Exit 1 means a PR errored, or --apply left an eligible PR blocked. Skipped PRs
do not fail the run. Invalid arguments exit 2; interruption exits 130.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := output.resolve(cmd)
			if err != nil {
				return err
			}
			if (len(args) == 0 && !cmd.Flags().Changed("repo")) || (len(args) > 0 && cmd.Flags().Changed("repo")) {
				return usageError{errors.New("provide PR references or --repo owner/name, exclusively")}
			}
			if attempts < 1 || waitTimeout <= 0 {
				return usageError{errors.New("--max-attempts and --wait-timeout must be positive")}
			}
			if cmd.Flags().Changed("merge-method") && opts.MergeMethod != "squash" && opts.MergeMethod != "merge" && opts.MergeMethod != "rebase" {
				return usageError{errors.New("--merge-method needs squash, merge or rebase")}
			}
			var owner, name string
			if cmd.Flags().Changed("repo") {
				owner, name, err = github.ParseRepository(repository)
				if err != nil {
					return usageError{err}
				}
			}
			targets, seen := []github.PRTarget{}, map[string]bool{}
			for _, arg := range args {
				target, parseErr := github.ParsePRTarget(arg)
				if parseErr != nil {
					return usageError{parseErr}
				}
				key := strings.ToLower(target.String())
				if seen[key] {
					return usageError{fmt.Errorf("PR %s was selected twice", target)}
				}
				seen[key] = true
				targets = append(targets, target)
			}
			nameAgent := strings.ToLower(agentName)
			if cmd.Flags().Changed("agent") && !config.ValidAgent(nameAgent) {
				return usageError{fmt.Errorf("--agent needs %s", config.AgentList())}
			}
			cfg, err := loadPRConfig()
			if err != nil {
				return err
			}
			if nameAgent == "" {
				nameAgent = cfg.Use
			}
			if opts.Apply && nameAgent == "api" {
				return usageError{errors.New("pr handle --apply needs --agent claude or --agent codex; api supports preview only")}
			}
			if !prCommandsAvailable {
				return errors.New("pr commands are unavailable in the demo build")
			}
			client, err := newPRHandleClient()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			context.AfterFunc(ctx, stop)
			if owner != "" {
				targets, err = client.OpenPRs(ctx, owner, name)
				if err != nil {
					if ctx.Err() != nil {
						return errInterrupted
					}
					return err
				}
			}
			agent := &handleTaskAgent{cfg: cfg, name: nameAgent}
			results := []pr.HandleResult{}
			failed := false
			for _, target := range targets {
				if ctx.Err() != nil {
					break
				}
				// Every PR has its own private checkout, including failed repairs.
				result, runErr := func() (pr.HandleResult, error) {
					root, err := os.MkdirTemp("", "deps-handler-handle-")
					if err != nil {
						return pr.HandleResult{Target: target, Status: "error", Summary: err.Error(), Error: err.Error()}, err
					}
					service := pr.HandleService{Client: client, Agent: agent, Workspace: newPRWorkspace(filepath.Join(root, "checkout")),
						AgentTimeout: cfg.Timeout, WaitTimeout: waitTimeout, PollInterval: 10 * time.Second, MaxAttempts: attempts,
						Progress: func(message string) { fmt.Fprintln(cmd.ErrOrStderr(), review.Clean(message)) },
					}
					result, runErr := service.Run(ctx, target, opts)
					if opts.Apply && (runErr != nil || result.Status == "blocked") && result.Workspace != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), "Repair checkout kept at", result.Workspace)
					} else {
						_ = os.RemoveAll(root)
						result.Workspace = ""
					}
					return result, runErr
				}()
				results = append(results, result)
				failed = failed || runErr != nil || (opts.Apply && result.Status != "merged" && result.Status != "skipped")
			}
			if format == "json" {
				data, err := json.MarshalIndent(results, "", "  ")
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), review.Clean(string(data))); err != nil {
					return err
				}
			} else if err := pr.HandleReport(cmd.OutOrStdout(), results, opts.Apply, format == "list"); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			if failed {
				return errors.New("some PRs could not be handled; see their reported blockers and next steps")
			}
			return nil
		},
	}
	c.Flags().StringVarP(&repository, "repo", "r", "", "select all open PRs in this owner/repo instead of explicit PR references")
	c.Flags().StringVar(&agentName, "agent", "", "agent: "+config.AgentList()+" (default from config, otherwise claude)")
	c.Flags().BoolVar(&opts.Apply, "apply", false, "authorize repairs, pushes, supported CI reruns and merging (default: read-only preview)")
	c.Flags().StringVar(&opts.MergeMethod, "merge-method", "", "require squash, merge or rebase (default: first enabled in that order)")
	c.Flags().IntVar(&attempts, "max-attempts", 3, "maximum combined repair/rerun attempts per PR, plus a final review")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 15*time.Minute, "maximum wait for CI per head or rerun")
	output = addOutputFlags(c, false)
	return c
}
