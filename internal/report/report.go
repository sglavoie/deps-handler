// Package report renders run results for humans or as JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sglavoie/deps-merger/internal/config"
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
// their pull requests and the report is colored.
func Repo(w io.Writer, apply bool, res runner.RepoResult) {
	repo(w, apply, res, styleFor(w))
}

func repo(w io.Writer, apply bool, res runner.RepoResult, s style) {
	p := res.Policy
	checks := "checks required"
	if !p.RequireChecks {
		checks = "checks optional"
	} else if p.MergeWithoutChecksAfter > 0 {
		checks = fmt.Sprintf("checks required unless none after %s", config.ShortDuration(p.MergeWithoutChecksAfter))
	}
	method := p.MergeMethod
	if res.MergeMethod != "" && res.MergeMethod != p.MergeMethod {
		method = fmt.Sprintf("%s; %s not allowed", res.MergeMethod, p.MergeMethod)
	}
	header := fmt.Sprintf("(%s, up to %s, %s", method, p.MaxUpdateType, checks)
	if !p.AgentReview {
		header += ", no agent review"
	}
	if res.Permission != "" {
		header += ", " + strings.ToLower(res.Permission) + " access"
	}
	fmt.Fprintln(w, s.paint(bold, res.Repo)+"  "+s.paint(dim, header+")"))
	if res.Error != "" {
		fmt.Fprintf(w, "  %s %s\n\n", s.paint(red, "error:"), res.Error)
		return
	}
	decisions := pending(res)
	if !HasPRs(res) {
		fmt.Fprintln(w, "  "+s.paint(dim, "no open Dependabot PRs"))
		fmt.Fprintln(w)
		return
	}

	var rows [][]cell
	for _, ev := range res.Events {
		k := string(ev.Kind)
		rows = append(rows, []cell{{text: fmt.Sprintf("#%d", ev.Number)}, {text: ev.Grade.String(), color: gradeColor(ev.Grade)}, {text: k, color: labelColor(k)}, {text: ev.Detail}, {text: truncate(ev.Title), url: ev.URL}})
	}
	for _, d := range decisions {
		l := label(d.Action, apply)
		rows = append(rows, []cell{{text: fmt.Sprintf("#%d", d.Number)}, {text: d.Grade.String(), color: gradeColor(d.Grade)}, {text: l, color: labelColor(l)}, {text: d.Reason}, {text: truncate(d.Title), url: d.URL}})
	}
	table(w, s, "  ", rows)
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "  %s %s\n", s.paint(yellow, "warning:"), warning)
	}

	if len(res.Findings) > 0 {
		fmt.Fprintln(w, "  "+s.paint(bold+";"+yellow, "needs attention:"))
		for _, f := range res.Findings {
			fmt.Fprintf(w, "    %s %s\n", s.paint(yellow, "-"), f.Summary)
			if f.Kind == policy.FindingRepoCheck || f.Kind == policy.FindingNoChecks || f.Kind == policy.FindingReviewRequired {
				fmt.Fprintf(w, "      %s %s\n", s.paint(dim, "PRs:"), prList(f.PRs))
			}
			for _, d := range f.Details {
				fmt.Fprintf(w, "      %s\n", s.paint(dim, "- "+d))
			}
			for _, sg := range f.Suggestions {
				fmt.Fprintf(w, "      %s %s\n", s.paint(cyan, "suggest:"), sg)
			}
		}
	}
	fmt.Fprintln(w)
}

// HasPRs reports whether a repository had open Dependabot PRs to report,
// or failed before it could tell.
func HasPRs(res runner.RepoResult) bool {
	return res.Error != "" || len(res.Decisions) > 0 || len(res.Events) > 0
}

// Progress writes a one-line note that a repository is done, for runs whose
// report only comes at the end.
func Progress(w io.Writer, res runner.RepoResult) {
	s := styleFor(w)
	if res.Error != "" {
		fmt.Fprintf(w, "%s: %s %s\n", res.Repo, s.paint(red, "error:"), res.Error)
		return
	}
	fmt.Fprintf(w, "%s: %s (%s)\n", res.Repo, s.paint(green, "done"), plural(len(res.Decisions), "PR"))
}

// Summary writes the totals line across all repositories.
func Summary(w io.Writer, apply bool, results []runner.RepoResult) {
	summary(w, apply, results, styleFor(w))
}

func summary(w io.Writer, apply bool, results []runner.RepoResult, s style) {
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
	for _, k := range []string{"merged", "merge-failed", "merge", "would merge", "rebase-requested", "rebase-failed", "rebase", "would rebase", "review", "wait", "skip"} {
		if counts[k] > 0 {
			parts = append(parts, s.paint(labelColor(k), fmt.Sprintf("%d %s", counts[k], k)))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, s.paint(dim, "nothing to do"))
	}
	line := s.paint(bold, fmt.Sprintf("%d repos:", len(results))) + " " + strings.Join(parts, ", ")
	if findings > 0 {
		line += "; " + s.paint(yellow, fmt.Sprintf("%d need attention", findings))
	}
	if failed > 0 {
		line += "; " + s.paint(red, fmt.Sprintf("%d failed", failed))
	}
	fmt.Fprintln(w, line)
	if !apply && (counts["would merge"] > 0 || counts["would rebase"] > 0) {
		fmt.Fprintln(w, s.paint(dim, "dry run: nothing was changed; rerun with --apply to merge"))
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

// plural writes n things, as "1 PR" or "2 PRs".
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

func prList(prs []int) string {
	parts := make([]string, len(prs))
	for i, n := range prs {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}
