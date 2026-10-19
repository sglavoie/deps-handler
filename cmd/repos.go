package cmd

import (
	"fmt"
	"text/tabwriter"

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
			fmt.Fprintln(tw, "REPO\tMERGE\tMAX UPDATE\tREQUIRE CHECKS")
			for _, r := range cfg.Repos {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\n", r.FullName(), r.Policy.MergeMethod, r.Policy.MaxUpdateType, r.Policy.RequireChecks)
			}
			return tw.Flush()
		},
	}
}
