package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/spf13/cobra"
)

func newReposCmd() *cobra.Command {
	var (
		asJSON bool
		repos  []string
		owners []string
	)
	c := &cobra.Command{
		Use:   "repos",
		Short: "List configured repositories and their effective policy",
		Long: `List the configured repositories, skipped ones included, with the policy
each one runs under, and the agent --review uses.

With --repo and --owner, only the named repositories and the repositories
of the named owners are listed, as run selects them.

The policy shown is the config's alone: run also reads each repository's
.github/deps-merger.yml and deps-merger.yml in its owner's .github
repository, which can override it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkSelection(cmd, repos, owners); err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			// Skipped repos are listed: the SKIP column says so.
			if cfg.Repos, err = cfg.Filter(repos, owners, true); err != nil {
				return usageError{err}
			}
			if asJSON {
				return reposJSON(cmd.OutOrStdout(), cfg)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "REPO\tMERGE\tMAX UPDATE\tREQUIRE CHECKS\tNO-CHECKS MERGE\tAGENT REVIEW\tDEPENDABOT COMMANDS\tSKIP")
			for _, r := range cfg.Repos {
				noChecks := "never"
				switch d := r.Policy.MergeWithoutChecksAfter; {
				case !r.Policy.RequireChecks:
					noChecks = "-" // checks are optional anyway
				case d > 0:
					noChecks = "after " + config.ShortDuration(d)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.FullName(), r.Policy.MergeMethod, r.Policy.MaxUpdateType, yesNo(r.Policy.RequireChecks), noChecks, yesNo(r.Policy.AgentReview), yesNo(r.Policy.DependabotCommands), yesNo(r.Skip))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nagent for --review: "+agentSummary(cfg.Agent))
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print the repositories and the agent as JSON")
	c.Flags().StringSliceVarP(&repos, "repo", "r", nil, "only list these repos (owner/name or name; repeatable)")
	c.Flags().StringSliceVar(&owners, "owner", nil, "only list the configured repos of these owners (repeatable)")
	completeSelection(c)
	return c
}

// reposJSON writes the configured repositories, with their effective
// policy as run --json reports it, and the agent as configured.
func reposJSON(w io.Writer, cfg *config.Config) error {
	type repo struct {
		Repo   string        `json:"repo"`
		Policy config.Policy `json:"policy"`
		Skip   bool          `json:"skip"`
	}
	type agent struct {
		Use     string `json:"use"`
		Timeout string `json:"timeout"`
		Claude  struct {
			Model        string  `json:"model"`
			ConfigDir    string  `json:"config_dir"`
			MaxBudgetUSD float64 `json:"max_budget_usd"`
		} `json:"claude"`
		Codex struct {
			Model string `json:"model"`
			Home  string `json:"home"`
		} `json:"codex"`
	}
	doc := struct {
		Repos []repo `json:"repos"`
		Agent agent  `json:"agent"`
	}{Repos: make([]repo, 0, len(cfg.Repos))}
	for _, r := range cfg.Repos {
		doc.Repos = append(doc.Repos, repo{r.FullName(), r.Policy, r.Skip})
	}
	a := cfg.Agent
	doc.Agent.Use, doc.Agent.Timeout = a.Use, config.ShortDuration(a.Timeout)
	doc.Agent.Claude.Model, doc.Agent.Claude.ConfigDir, doc.Agent.Claude.MaxBudgetUSD = a.Claude.Model, a.Claude.ConfigDir, a.Claude.MaxBudgetUSD
	doc.Agent.Codex.Model, doc.Agent.Codex.Home = a.Codex.Model, a.Codex.Home
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
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
