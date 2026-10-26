// Package report renders run results for humans or as JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/runner"
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
	// Everything printed goes through paint, which makes it Plain: reasons
	// and findings can quote an agent's answer or GitHub's errors.
	fmt.Fprintln(w, s.paint(bold, res.Repo)+"  "+s.paint(dim, header+")"))
	// A repository that failed part way still shows the writes made and
	// the warnings raised before then: they happened. Its decisions and
	// findings come from a read the failure may have left stale.
	var decisions []policy.Decision
	if res.Error != "" {
		fmt.Fprintf(w, "  %s %s\n", s.paint(red, "error:"), s.paint("", res.Error))
	} else {
		decisions = pending(res)
		if len(res.Decisions) == 0 && len(res.Events) == 0 {
			fmt.Fprintln(w, "  "+s.paint(dim, "no open Dependabot PRs"))
		}
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
		fmt.Fprintf(w, "  %s %s\n", s.paint(yellow, "warning:"), s.paint("", warning))
	}

	if res.Error == "" && len(res.Findings) > 0 {
		fmt.Fprintln(w, "  "+s.paint(bold+";"+yellow, "needs attention:"))
		for _, f := range res.Findings {
			fmt.Fprintf(w, "    %s %s\n", s.paint(yellow, "-"), s.paint("", f.Summary))
			if f.Kind == policy.FindingRepoCheck || f.Kind == policy.FindingNoChecks || f.Kind == policy.FindingReviewRequired {
				fmt.Fprintf(w, "      %s %s\n", s.paint(dim, "PRs:"), prList(f.PRs))
			}
			for _, d := range f.Details {
				fmt.Fprintf(w, "      %s\n", s.paint(dim, "- "+d))
			}
			for _, sg := range f.Suggestions {
				fmt.Fprintf(w, "      %s %s\n", s.paint(cyan, "suggest:"), s.paint("", sg))
			}
		}
	}
	fmt.Fprintln(w)
}

// HasPRs reports whether a repository had open Dependabot PRs to report,
// or failed before it could tell. One with a warning counts too: the
// warning may be the run's, such as a verdict cache found unreadable as it
// started, and must not be hidden with the repository.
func HasPRs(res runner.RepoResult) bool {
	return res.Error != "" || len(res.Decisions) > 0 || len(res.Events) > 0 || len(res.Warnings) > 0
}

// Progress writes a one-line note that a repository is done, for runs whose
// report only comes at the end.
func Progress(w io.Writer, res runner.RepoResult) {
	progress(w, res, styleFor(w))
}

func progress(w io.Writer, res runner.RepoResult, s style) {
	switch {
	case res.Error != "":
		fmt.Fprintf(w, "%s: %s %s\n", res.Repo, s.paint(red, "error:"), s.paint("", res.Error))
	case res.Failed():
		// A write or a review failed: the JSON says which.
		fmt.Fprintf(w, "%s: %s (%s)\n", res.Repo, s.paint(red, "failed"), plural(len(res.Decisions), "PR"))
	default:
		fmt.Fprintf(w, "%s: %s (%s)\n", res.Repo, s.paint(green, "done"), plural(len(res.Decisions), "PR"))
	}
}

// Summary writes the totals line across the repositories reported. hidden
// counts those left out of the report for having no open Dependabot PRs.
func Summary(w io.Writer, apply bool, results []runner.RepoResult, hidden int) {
	summary(w, apply, results, hidden, styleFor(w))
}

func summary(w io.Writer, apply bool, results []runner.RepoResult, hidden int, s style) {
	counts := map[string]int{}
	findings, failed := 0, 0
	for _, r := range results {
		// Failed as the exit status counts it: a failed write or review
		// fails the run as much as a repository that could not be read.
		if r.Failed() {
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
			parts = append(parts, s.paint(labelColor(k), fmt.Sprintf("%d %s", counts[k], summaryLabel(k))))
		}
	}
	if len(results) == 0 && hidden > 0 {
		// --open left nothing to show: say why, rather than count none.
		fmt.Fprintln(w, s.paint(dim, "no open Dependabot PRs in "+plural(hidden, "repo")))
		return
	}
	if len(parts) == 0 {
		parts = append(parts, s.paint(dim, "nothing to do"))
	}
	head := plural(len(results), "repo")
	if hidden > 0 {
		head += fmt.Sprintf(" (%d without open PRs hidden)", hidden)
	}
	line := s.paint(bold, head+":") + " " + strings.Join(parts, ", ")
	if findings > 0 {
		verb := "need"
		if findings == 1 {
			verb = "needs"
		}
		line += "; " + s.paint(yellow, fmt.Sprintf("%d %s attention", findings, verb))
	}
	if failed > 0 {
		line += "; " + s.paint(red, plural(failed, "repo")+" failed")
	}
	fmt.Fprintln(w, line)
	if !apply {
		var todo []string
		if counts["would merge"] > 0 {
			todo = append(todo, "merge")
		}
		if counts["would rebase"] > 0 {
			todo = append(todo, "rebase")
		}
		if len(todo) > 0 {
			fmt.Fprintln(w, s.paint(dim, "dry run: nothing was changed; rerun with --apply to "+strings.Join(todo, " and ")))
		}
	}
}

// summaryLabel words an action or event for the totals line, where the
// event kinds JSON uses ("rebase-requested") and the bare actions ("skip")
// would read as code.
func summaryLabel(k string) string {
	switch k {
	case "merge-failed":
		return "merge failed"
	case "merge":
		return "left to merge"
	case "rebase-requested":
		return "rebase requested"
	case "rebase-failed":
		return "rebase failed"
	case "rebase":
		return "left to rebase"
	case "review":
		return "awaiting review"
	case "wait":
		return "waiting"
	case "skip":
		return "skipped"
	}
	return k
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
