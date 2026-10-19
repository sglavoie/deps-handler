package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/cli/go-gh/v2/pkg/term"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/report"
	"github.com/sglavoie/deps-merger/internal/runner"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var (
		apply  bool
		asJSON bool
		repos  []string
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

Exits 1 if a repository could not be processed or a merge or rebase request
failed, and 130 when interrupted.`,
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
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client, err := github.New()
			if err != nil {
				return err
			}
			viewer, err := client.Viewer(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return errInterrupted
				}
				return err
			}

			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			var progress func(runner.RepoResult)
			if !asJSON {
				progress = func(r runner.RepoResult) { report.Repo(out, apply, r) }
			} else if f, ok := errOut.(*os.File); ok && term.IsTerminal(f) {
				// JSON comes at the end; say what is happening meanwhile,
				// unless stderr is captured along with it.
				progress = func(r runner.RepoResult) { report.Progress(errOut, r) }
			}
			results := runner.New(client, viewer, apply).Run(ctx, selected, progress)
			if asJSON {
				if err := report.JSON(out, apply, results); err != nil {
					return err
				}
			} else {
				report.Summary(out, apply, results)
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
	return c
}

var errInterrupted = errors.New("interrupted")
