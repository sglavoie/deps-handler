package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/report"
	"github.com/sglavoie/deps-handler/pkg/review"
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

HEAD is the commit a rejection was given for: it holds for that commit
only. An approval holds for any head, unless Dependabot records no
versions for the update (a Docker digest, an action pinned by commit):
its key then holds the commit reviewed, which HEAD shows.

A target narrows the list to a repository (owner/name) or a pull request
(owner/name#7). Exits 1 if a target matched nothing.`,
		Args: func(_ *cobra.Command, targets []string) error {
			for _, t := range targets {
				if !targetPattern.MatchString(t) {
					return fmt.Errorf("%q is neither a command nor a target (owner/name or owner/name#7); the command is \"verdicts forget\"", t)
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, targets []string) error {
			cache, err := openVerdicts(cmd)
			if err != nil {
				return err
			}
			entries := cache.Entries()
			unmatched := 0
			if len(targets) > 0 {
				for _, t := range targets {
					if !slices.ContainsFunc(entries, func(e review.Entry) bool { return review.Matches(e.Key, t) }) {
						fmt.Fprintf(cmd.ErrOrStderr(), "deps-handler: no cached verdict matches %q\n", t)
						unmatched++
					}
				}
				entries = slices.DeleteFunc(entries, func(e review.Entry) bool {
					return !slices.ContainsFunc(targets, func(t string) bool { return review.Matches(e.Key, t) })
				})
			}
			if err := printVerdicts(cmd, entries, asJSON); err != nil {
				return err
			}
			if unmatched > 0 {
				return exitError{code: 1}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print verdicts as JSON, with their concerns")
	c.ValidArgsFunction = completeTargets
	c.AddCommand(&cobra.Command{
		Use:   "forget TARGET...",
		Short: "Drop cached verdicts so the next --review run judges them again",
		Long: `Drop cached verdicts so the next --review run judges those updates again.
A target is a repository (owner/name), a pull request (owner/name#7) or an
update as "verdicts" lists it. Exits 1 if a target matched nothing.`,
		ValidArgsFunction: completeTargets,
		Args: func(cmd *cobra.Command, targets []string) error {
			if err := cobra.MinimumNArgs(1)(cmd, targets); err != nil {
				return err
			}
			for _, t := range targets {
				if !targetPattern.MatchString(t) && !updatePattern.MatchString(t) {
					return fmt.Errorf("%q is not a target: want owner/name, owner/name#7 or an update as \"verdicts\" lists it", t)
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, targets []string) error {
			cache, err := openVerdicts(cmd)
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
					fmt.Fprintf(cmd.ErrOrStderr(), "deps-handler: no cached verdict matches %q\n", t)
					unmatched++
					continue
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: forgot %d verdict(s)\n", t, n); err != nil {
					return err
				}
			}
			if unmatched > 0 {
				return exitError{code: 1}
			}
			return nil
		},
	})
	return c
}

// completeTargets completes the repositories and pull requests the cached
// verdicts are for, as owner/name and owner/name#7, leaving out those
// already given.
func completeTargets(_ *cobra.Command, given []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	cache, err := review.OpenCache(verdictsPath(stateDir))
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []cobra.Completion
	for _, e := range cache.Entries() {
		pr, _, _ := strings.Cut(e.Key, " ")
		repo, _, _ := strings.Cut(pr, "#")
		for _, t := range []string{repo, pr} {
			if !slices.Contains(out, t) && !slices.Contains(given, t) {
				out = append(out, t)
			}
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// targetPattern is a repository (owner/name) or a pull request
// (owner/name#7).
var targetPattern = regexp.MustCompile(`^[^/#\s]+/[^/#\s]+(#[0-9]+)?$`)

// updatePattern is an update as a verdict's key names it: its pull request
// followed by the versions or the commit reviewed.
var updatePattern = regexp.MustCompile(`^[^/#\s]+/[^/#\s]+#[0-9]+ \S`)

func printVerdicts(cmd *cobra.Command, entries []review.Entry, asJSON bool) error {
	if asJSON {
		return verdictsJSON(cmd, entries)
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no cached verdicts")
		return err
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UPDATE\tVERDICT\tAGENT\tHEAD\tREVIEWED\tEXPIRES\tSUMMARY")
	for _, e := range entries {
		// The cache file may have been edited: nothing in it reaches the
		// terminal as escape sequences.
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", report.Plain(e.Key), verdictWord(e), report.Plain(e.Agent), report.Plain(heldFor(e)), e.ReviewedAt.Local().Format("2006-01-02 15:04"), e.Expires.Local().Format("2006-01-02"), clipSummary(e.Verdict.Summary))
	}
	return tw.Flush()
}

// heldFor is the head a verdict holds for, shortened: a rejection holds
// for the commit reviewed only.
func heldFor(e review.Entry) string { return fmt.Sprintf("%.7s", heldForOID(e)) }

// heldForOID is the head a verdict holds for: the commit a rejection was
// given for, or "any" for an approval, which survives a rebase unless the
// update has no versions and its key holds the head.
func heldForOID(e review.Entry) string {
	if e.Verdict.Approved && !e.PinsHead() || e.HeadOID == "" {
		return "any"
	}
	return e.HeadOID
}

func verdictWord(e review.Entry) string {
	if e.Verdict.Approved {
		return "approved"
	}
	return "rejected"
}

// clipSummary keeps the table readable: the full text is in --json.
func clipSummary(s string) string {
	s = strings.Join(strings.Fields(report.Plain(s)), " ")
	if r := []rune(s); len(r) > summaryWidth {
		return string(r[:summaryWidth-1]) + "…"
	}
	return s
}

func verdictsJSON(cmd *cobra.Command, entries []review.Entry) error {
	type verdict struct {
		Update     string    `json:"update"`
		Verdict    string    `json:"verdict"`
		Approved   bool      `json:"approved"`
		Agent      string    `json:"agent"`
		HeadOID    string    `json:"head_oid"`
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
		out = append(out, verdict{e.Key, verdictWord(e), e.Verdict.Approved, e.Agent, heldForOID(e), e.ReviewedAt, e.Expires, e.Verdict.Summary, concerns})
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	// An object, as run and repos print, so fields can be added later.
	return enc.Encode(struct {
		Verdicts []verdict `json:"verdicts"`
	}{out})
}

// openVerdicts opens the verdict cache, warning on cmd's stderr when it
// had to move an unreadable cache file aside.
func openVerdicts(cmd *cobra.Command) (*review.Cache, error) {
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	cache, err := review.OpenCache(verdictsPath(stateDir))
	if err != nil {
		return nil, err
	}
	if msg := cache.TakeRecovered(); msg != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "deps-handler: warning:", msg)
	}
	return cache, nil
}
