package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
	"github.com/spf13/cobra"
)

var newPRRerunClient = github.NewActionsClient

func newPRRerunCmd() *cobra.Command {
	var opts pr.RerunOptions
	var output *outputFormat
	var waitTimeout time.Duration
	c := &cobra.Command{
		Use:   "rerun <owner/repo#number | PR-URL>",
		Short: "Preview failed Actions jobs; --apply reruns once and watches the new attempt",
		Long: `Preview verified failed GitHub Actions jobs on the current PR head.
With --apply, request one rerun per selected workflow run and wait for its
new attempt. Without --job, rerun all failed jobs in each listed workflow.
With --job, rerun that Actions job only. GitHub also reruns dependent jobs.

This command does not use an AI agent or require a project config. It does
not determine whether a failure is transient. Resolve human blockers first.
--head and --attempt guard commands copied from an earlier diagnosis.
Checks must identify the PR head and a completed, current workflow attempt.
External checks and unidentified jobs are reported without rerunning them.
Requests are never retried automatically; inspect Actions after an
unconfirmed response before deciding whether to try again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, formatErr := output.resolve(cmd)
			if formatErr != nil {
				return formatErr
			}
			target, err := github.ParsePRTarget(args[0])
			if err != nil {
				return usageError{err}
			}
			if waitTimeout <= 0 || (cmd.Flags().Changed("job") && opts.JobID <= 0) || (cmd.Flags().Changed("attempt") && opts.Attempt <= 0) || (opts.Attempt > 0 && opts.JobID == 0) {
				return usageError{errors.New("--wait-timeout, --job and --attempt must be positive; --attempt requires --job")}
			}
			if cmd.Flags().Changed("head") && strings.TrimSpace(opts.HeadOID) == "" {
				return usageError{errors.New("--head needs a commit SHA")}
			}
			if !prCommandsAvailable {
				return errors.New("pr commands are unavailable in the demo build")
			}
			client, err := newPRRerunClient()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			service := pr.RerunService{Client: client, WaitTimeout: waitTimeout, PollInterval: 10 * time.Second,
				Progress: func(message string) { fmt.Fprintln(cmd.ErrOrStderr(), review.Clean(message)) },
			}
			result, runErr := service.Run(ctx, target, opts)
			if format == "json" {
				data, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), review.Clean(string(data))); err != nil {
					return err
				}
			} else {
				render := pr.RerunTable
				if format == "list" {
					render = pr.RerunMarkdown
				}
				if err := render(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			if runErr != nil {
				return errors.New(review.Clean(runErr.Error()))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&opts.Apply, "apply", false, "request reruns and verify their new attempts (default: preview only)")
	c.Flags().Int64Var(&opts.JobID, "job", 0, "select one Actions job ID, rather than all failed jobs")
	c.Flags().StringVar(&opts.HeadOID, "head", "", "require the PR head SHA from a saved diagnosis or preview")
	c.Flags().IntVar(&opts.Attempt, "attempt", 0, "require this previous workflow attempt (requires --job)")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 15*time.Minute, "maximum wait for each new workflow attempt")
	output = addOutputFlags(c, false)
	return c
}
