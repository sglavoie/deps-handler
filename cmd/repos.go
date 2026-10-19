package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/spf13/cobra"
)

func newReposCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repos",
		Short: "List configured repositories and their effective policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "REPO\tMERGE\tMAX UPDATE\tREQUIRE CHECKS\tNO-CHECKS MERGE\tAGENT REVIEW\tSKIP")
			for _, r := range cfg.Repos {
				noChecks := "never"
				if d := r.Policy.MergeWithoutChecksAfter; d > 0 {
					noChecks = "after " + config.ShortDuration(d)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%t\t%t\n", r.FullName(), r.Policy.MergeMethod, r.Policy.MaxUpdateType, r.Policy.RequireChecks, noChecks, r.Policy.AgentReview, r.Skip)
			}
			return tw.Flush()
		},
	}
}
