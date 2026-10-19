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
				switch d := r.Policy.MergeWithoutChecksAfter; {
				case !r.Policy.RequireChecks:
					noChecks = "-" // checks are optional anyway
				case d > 0:
					noChecks = "after " + config.ShortDuration(d)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%t\t%t\n", r.FullName(), r.Policy.MergeMethod, r.Policy.MaxUpdateType, r.Policy.RequireChecks, noChecks, r.Policy.AgentReview, r.Skip)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nagent for --review: "+agentSummary(cfg.Agent))
			return nil
		},
	}
}

// agentSummary describes the configured agent and its limits.
func agentSummary(a config.Agent) string {
	model, extra := a.Codex.Model, ""
	if a.Use == "claude" {
		model = a.Claude.Model
		if a.Claude.MaxBudgetUSD > 0 {
			extra = fmt.Sprintf(", up to $%g per review", a.Claude.MaxBudgetUSD)
		}
	}
	if model == "" {
		model = "default model"
	}
	return fmt.Sprintf("%s (%s, %s timeout%s)", a.Use, model, config.ShortDuration(a.Timeout), extra)
}
