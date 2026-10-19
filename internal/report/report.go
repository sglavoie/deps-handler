// Package report renders run results for humans or as JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/cli/go-gh/v2/pkg/term"
	"github.com/sglavoie/deps-merger/internal/policy"
	"github.com/sglavoie/deps-merger/internal/runner"
)

const titleWidth = 60

// JSON writes results as an indented JSON document.
func JSON(w io.Writer, apply bool, results []runner.RepoResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Apply   bool                `json:"apply"`
		Results []runner.RepoResult `json:"results"`
	}{apply, results})
}

// Repo writes one repository's section. On a terminal, PR titles link to
// their pull requests.
func Repo(w io.Writer, apply bool, res runner.RepoResult) {
	f, ok := w.(*os.File)
	repo(w, apply, res, ok && term.IsTerminal(f))
}

func repo(w io.Writer, apply bool, res runner.RepoResult, links bool) {
	title := func(url, s string) string {
		s = truncate(s)
		if !links || url == "" {
			return s
		}
		// OSC 8 hyperlink; the title is the last cell, so the escape
		// codes do not upset column widths.
		return "\x1b]8;;" + url + "\x1b\\" + s + "\x1b]8;;\x1b\\"
	}
	p := res.Policy
	checks := "checks required"
	if !p.RequireChecks {
		checks = "checks optional"
	}
	method := p.MergeMethod
	if res.MergeMethod != "" && res.MergeMethod != p.MergeMethod {
		method = fmt.Sprintf("%s; %s not allowed", res.MergeMethod, p.MergeMethod)
	}
	header := fmt.Sprintf("%s  (%s, up to %s, %s", res.Repo, method, p.MaxUpdateType, checks)
	if res.Permission != "" {
		header += ", " + strings.ToLower(res.Permission) + " access"
	}
	fmt.Fprintln(w, header+")")
	if res.Error != "" {
		fmt.Fprintf(w, "  error: %s\n\n", res.Error)
		return
	}
	decisions := pending(res)
	if len(decisions) == 0 && len(res.Events) == 0 {
		fmt.Fprintln(w, "  no open Dependabot PRs")
		fmt.Fprintln(w)
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, ev := range res.Events {
		fmt.Fprintf(tw, "  #%d\t%s\t%s\t%s\t%s\n", ev.Number, ev.Grade, ev.Kind, ev.Detail, title(ev.URL, ev.Title))
	}
	for _, d := range decisions {
		fmt.Fprintf(tw, "  #%d\t%s\t%s\t%s\t%s\n", d.Number, d.Grade, label(d.Action, apply), d.Reason, title(d.URL, d.Title))
	}
	tw.Flush()

	if len(res.Findings) > 0 {
		fmt.Fprintln(w, "  needs attention:")
		for _, f := range res.Findings {
			fmt.Fprintf(w, "    - %s\n", f.Summary)
			if f.Kind == policy.FindingRepoCheck || f.Kind == policy.FindingNoChecks || f.Kind == policy.FindingReviewRequired {
				fmt.Fprintf(w, "      PRs: %s\n", prList(f.PRs))
			}
			for _, s := range f.Suggestions {
				fmt.Fprintf(w, "      suggest: %s\n", s)
			}
		}
	}
	fmt.Fprintln(w)
}

// Progress writes a one-line note that a repository is done, for runs whose
// report only comes at the end.
func Progress(w io.Writer, res runner.RepoResult) {
	if res.Error != "" {
		fmt.Fprintf(w, "%s: error: %s\n", res.Repo, res.Error)
		return
	}
	fmt.Fprintf(w, "%s: done (%d PRs)\n", res.Repo, len(res.Decisions))
}

// Summary writes the totals line across all repositories.
func Summary(w io.Writer, apply bool, results []runner.RepoResult) {
	counts := map[string]int{}
	findings, failed := 0, 0
	for _, r := range results {
		if r.Error != "" {
			failed++
		}
		findings += len(r.Findings)
		for _, ev := range r.Events {
			counts[string(ev.Kind)]++
		}
		for _, d := range pending(r) {
			counts[label(d.Action, apply)]++
		}
	}
	var parts []string
	for _, k := range []string{"merged", "merge-failed", "merge", "would merge", "rebase-requested", "rebase-failed", "rebase", "would rebase", "wait", "skip"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing to do")
	}
	line := fmt.Sprintf("%d repos: %s", len(results), strings.Join(parts, ", "))
	if findings > 0 {
		line += fmt.Sprintf("; %d need attention", findings)
	}
	if failed > 0 {
		line += fmt.Sprintf("; %d failed", failed)
	}
	fmt.Fprintln(w, line)
	if !apply && (counts["would merge"] > 0 || counts["would rebase"] > 0) {
		fmt.Fprintln(w, "dry run: nothing was changed; rerun with --apply to merge")
	}
}

// pending returns the decisions for PRs this run did not act on. A PR with
// an event is shown by its event alone: after a failed merge its decision
// still says "merge", which reads as if it had merged.
func pending(res runner.RepoResult) []policy.Decision {
	acted := map[int]bool{}
	for _, ev := range res.Events {
		acted[ev.Number] = true
	}
	var out []policy.Decision
	for _, d := range res.Decisions {
		if !acted[d.Number] {
			out = append(out, d)
		}
	}
	return out
}

func label(a policy.Action, apply bool) string {
	switch a {
	case policy.ActionMerge:
		if !apply {
			return "would merge"
		}
		return "merge"
	case policy.ActionRebase:
		if !apply {
			return "would rebase"
		}
		return "rebase"
	}
	return string(a)
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= titleWidth {
		return s
	}
	return string(r[:titleWidth-1]) + "…"
}

func prList(prs []int) string {
	parts := make([]string, len(prs))
	for i, n := range prs {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}
