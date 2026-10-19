package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/review"
	"github.com/spf13/cobra"
)

func newVerdictsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "verdicts",
		Short: "List the agent verdicts kept between runs",
		Long: `List the agent verdicts kept between runs, one per update. A verdict is
reused for 14 days, until the PR moves to new versions. Use
"verdicts forget" to have an update reviewed again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cache, err := openVerdicts()
			if err != nil {
				return err
			}
			entries := cache.Entries()
			if len(entries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no cached verdicts")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UPDATE\tVERDICT\tAGENT\tREVIEWED\tSUMMARY")
			for _, e := range entries {
				verdict := "rejected"
				if e.Verdict.Approved {
					verdict = "approved"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Key, verdict, e.Agent, e.ReviewedAt.Local().Format("2006-01-02 15:04"), e.Verdict.Summary)
			}
			return tw.Flush()
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "forget TARGET...",
		Short: "Drop cached verdicts so the next --review run judges them again",
		Long: `Drop cached verdicts so the next --review run judges those updates again.
A target is a repository (owner/name), a pull request (owner/name#7) or an
update as "verdicts" lists it.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, targets []string) error {
			cache, err := openVerdicts()
			if err != nil {
				return err
			}
			for _, t := range targets {
				n, err := cache.Forget(t)
				if err != nil {
					return err
				}
				if n == 0 {
					return fmt.Errorf("no cached verdict matches %q", t)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: forgot %d verdict(s)\n", t, n)
			}
			return nil
		},
	})
	return c
}

func openVerdicts() (*review.Cache, error) {
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	return review.OpenCache(verdictsPath(stateDir))
}
