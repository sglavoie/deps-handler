package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/review"
	"github.com/spf13/cobra"
)

// summaryWidth caps the summary column of the verdicts table.
const summaryWidth = 70

func newVerdictsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "verdicts [TARGET...]",
		Short: "List the agent verdicts kept between runs",
		Long: `List the agent verdicts kept between runs, one per update. An approval is
reused for 14 days, until the PR moves to new versions; a rejection only
until Dependabot rebases the PR. Use "verdicts forget" to have an update
reviewed again.

A target narrows the list to a repository (owner/name) or a pull request
(owner/name#7).`,
		RunE: func(cmd *cobra.Command, targets []string) error {
			cache, err := openVerdicts()
			if err != nil {
				return err
			}
			entries := cache.Entries()
			if len(targets) > 0 {
				entries = slices.DeleteFunc(entries, func(e review.Entry) bool {
					return !slices.ContainsFunc(targets, func(t string) bool { return review.Matches(e.Key, t) })
				})
			}
			if asJSON {
				return verdictsJSON(cmd, entries)
			}
			if len(entries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no cached verdicts")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "UPDATE\tVERDICT\tAGENT\tREVIEWED\tEXPIRES\tSUMMARY")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Key, verdictWord(e), e.Agent, e.ReviewedAt.Local().Format("2006-01-02 15:04"), e.Expires.Local().Format("2006-01-02"), clipSummary(e.Verdict.Summary))
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print verdicts as JSON, with their concerns")
	c.AddCommand(&cobra.Command{
		Use:   "forget TARGET...",
		Short: "Drop cached verdicts so the next --review run judges them again",
		Long: `Drop cached verdicts so the next --review run judges those updates again.
A target is a repository (owner/name), a pull request (owner/name#7) or an
update as "verdicts" lists it. Exits 1 if a target matched nothing.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, targets []string) error {
			cache, err := openVerdicts()
			if err != nil {
				return err
			}
			unmatched := 0
			for _, t := range targets {
				n, err := cache.Forget(t)
				if err != nil {
					return err
				}
				if n == 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "deps-merger: no cached verdict matches %q\n", t)
					unmatched++
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: forgot %d verdict(s)\n", t, n)
			}
			if unmatched > 0 {
				return exitError{code: 1}
			}
			return nil
		},
	})
	return c
}

func verdictWord(e review.Entry) string {
	if e.Verdict.Approved {
		return "approved"
	}
	return "rejected"
}

// clipSummary keeps the table readable: the full text is in --json.
func clipSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > summaryWidth {
		return string(r[:summaryWidth-1]) + "…"
	}
	return s
}

func verdictsJSON(cmd *cobra.Command, entries []review.Entry) error {
	type verdict struct {
		Update     string    `json:"update"`
		Verdict    string    `json:"verdict"`
		Agent      string    `json:"agent"`
		ReviewedAt time.Time `json:"reviewed_at"`
		Expires    time.Time `json:"expires"`
		Summary    string    `json:"summary"`
		Concerns   []string  `json:"concerns"`
	}
	out := make([]verdict, 0, len(entries))
	for _, e := range entries {
		concerns := e.Verdict.Concerns
		if concerns == nil {
			concerns = []string{}
		}
		out = append(out, verdict{e.Key, verdictWord(e), e.Agent, e.ReviewedAt, e.Expires, e.Verdict.Summary, concerns})
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func openVerdicts() (*review.Cache, error) {
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	return review.OpenCache(verdictsPath(stateDir))
}
