package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/sglavoie/deps-handler/internal/diagnosiseval"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/report"
	"github.com/sglavoie/deps-handler/pkg/review"
	"github.com/spf13/cobra"
)

func reposTable(w io.Writer, cfg *config.Config) error {
	rows := make([][]string, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		noChecks := "never"
		switch d := r.Policy.MergeWithoutChecksAfter; {
		case !r.Policy.RequireChecks:
			noChecks = "-"
		case d > 0:
			noChecks = "after " + config.ShortDuration(d)
		}
		rows = append(rows, []string{r.FullName(), r.Policy.Mode, r.Policy.MergeMethod, r.Policy.MaxUpdateType.String(), yesNo(r.Policy.RequireChecks), noChecks, yesNo(r.Policy.AgentReview), yesNo(r.Policy.DependabotCommands), yesNo(r.Skip)})
	}
	if err := report.Table(w, []string{"REPO", "MODE", "MERGE", "MAX UPDATE", "REQUIRE CHECKS", "NO-CHECKS MERGE", "AGENT REVIEW", "DEPENDABOT COMMANDS", "SKIP"}, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\nagent for --review: "+report.Plain(agentSummary(cfg.Agent)))
	return err
}

func printVerdictsView(cmd *cobra.Command, entries []review.Entry, format string) error {
	if format != "table" || len(entries) == 0 {
		return printVerdicts(cmd, entries, format == "json")
	}
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, []string{e.Key, verdictWord(e), e.Agent, heldFor(e), e.ReviewedAt.Local().Format("2006-01-02 15:04"), e.Expires.Local().Format("2006-01-02"), clipSummary(e.Verdict.Summary)})
	}
	return report.Table(cmd.OutOrStdout(), []string{"UPDATE", "VERDICT", "AGENT", "HEAD", "REVIEWED", "EXPIRES", "SUMMARY"}, rows)
}

func writeEvalView(cmd *cobra.Command, result diagnosiseval.Report, format string) error {
	if format == "list" {
		return writeEvalReport(cmd, result)
	}
	rows := make([][]string, 0, len(result.Results))
	for _, c := range result.Results {
		status := "FAIL"
		if c.Passed {
			status = "PASS"
		}
		details := []string{}
		if c.Error != "" {
			details = append(details, "error: "+c.Error)
		}
		for _, finding := range c.Findings {
			details = append(details, fmt.Sprintf("%s [%s]: %s", finding.Kind, finding.Subject, finding.Message))
		}
		rows = append(rows, []string{c.ID, status, fmt.Sprintf("%d/%d", c.Score.CorrectActions, c.Score.ExpectedActions), fmt.Sprint(c.Score.UnsupportedCodeRepairs), fmt.Sprint(c.Score.MissedBlockers), strings.Join(details, "; ")})
	}
	if err := report.Table(cmd.OutOrStdout(), []string{"CASE", "RESULT", "ACTIONS", "UNSUPPORTED REPAIRS", "MISSED BLOCKERS", "DETAILS"}, rows); err != nil {
		return err
	}
	// Reuse the existing totals, without repeating individual case results.
	result.Results = nil
	return writeEvalReport(cmd, result)
}
