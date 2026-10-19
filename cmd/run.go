package cmd

import (
	"context"
	"os"
	"os/signal"

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
safe PRs that are behind their base branch.`,
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
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			client, err := github.New()
			if err != nil {
				return err
			}
			viewer, err := client.Viewer(ctx)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			var progress func(runner.RepoResult)
			if !asJSON {
				progress = func(r runner.RepoResult) { report.Repo(out, apply, r) }
			}
			results := runner.New(client, viewer, apply).Run(ctx, selected, progress)
			if asJSON {
				if err := report.JSON(out, apply, results); err != nil {
					return err
				}
			} else {
				report.Summary(out, apply, results)
			}
			for _, r := range results {
				if r.Error != "" {
					return exitError{"some repositories failed"}
				}
			}
			return ctx.Err()
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "merge safe PRs and request rebases (default is a dry run)")
	c.Flags().BoolVar(&asJSON, "json", false, "print results as JSON")
	c.Flags().StringSliceVarP(&repos, "repo", "r", nil, "only handle these repos (owner/name or name; repeatable)")
	return c
}
