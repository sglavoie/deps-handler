// Package report renders run results for humans or as JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/dependabot"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

const titleWidth = 60

// JSON writes results as an indented JSON document.
func JSON(w io.Writer, apply bool, results []runner.RepoResult) error {
	if results == nil {
		results = []runner.RepoResult{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Apply   bool                `json:"apply"`
		Results []runner.RepoResult `json:"results"`
	}{apply, results})
}

// Repo writes one repository's section. On a terminal, PR titles link to
// their pull requests and the report is colored.
func Repo(w io.Writer, apply bool, res runner.RepoResult) error {
	out := &errorWriter{w: w}
	repo(out, apply, res, styleFor(w))
	return out.err
}

// errorWriter keeps the first output error, even when rendering continues
// through several writes. A partial report must not count as success.
type errorWriter struct {
	w   io.Writer
	err error
}

func (w *errorWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func repo(w io.Writer, apply bool, res runner.RepoResult, s style) {
	apply = applies(apply, res)
	// Everything printed goes through paint, which makes it Plain: reasons
	// and findings can quote an agent's answer or GitHub's errors.
	fmt.Fprintln(w, s.paint(bold, res.Repo)+"  "+s.paint(dim, "("+header(res)+")"))
	repoBody(w, apply, res, s)
}

// header sums up the policy a repository ran under, and the access the
// viewer has to it.
func header(res runner.RepoResult) string {
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
	header := fmt.Sprintf("%s, up to %s, %s", method, p.MaxUpdateType, checks)
	if !p.AgentReview {
		header += ", no agent review"
	}
	if !p.DependabotCommands {
		header += ", no Dependabot commands"
	}
	if p.Mode == config.ModeReport {
		header += ", report only"
	}
	if res.Permission != "" {
		header += ", " + strings.ToLower(res.Permission) + " access"
	}
	return header
}

func repoBody(w io.Writer, apply bool, res runner.RepoResult, s style) {
	repoBodyView(w, apply, res, s, false)
}

func repoBodyTable(w io.Writer, apply bool, res runner.RepoResult, s style) {
	repoBodyView(w, applies(apply, res), res, s, true)
}

func repoBodyView(w io.Writer, apply bool, res runner.RepoResult, s style, bordered bool) {
	if len(res.ConfigFiles) > 0 {
		fmt.Fprintln(w, "  "+s.paint(dim, "policy from "+strings.Join(res.ConfigFiles, ", ")))
	}
	// A repository that failed part way still shows the writes made and
	// the warnings raised before then: they happened. Its decisions and
	// findings come from a read the failure may have left stale.
	var decisions []policy.Decision
	if res.Error != "" {
		fmt.Fprintf(w, "  %s %s\n", s.paint(red, "error:"), s.paint("", res.Error))
	} else {
		decisions = pending(res)
		if len(res.Decisions) == 0 && len(res.Events) == 0 && len(res.Findings) == 0 {
			fmt.Fprintln(w, "  "+s.paint(dim, "no open Dependabot PRs"))
		}
	}

	var rows [][]cell
	for _, ev := range res.Events {
		k := string(ev.Kind)
		title := ev.Title
		if bordered {
			title = compactTitle(title, ev.Dependencies)
		}
		rows = append(rows, []cell{{text: fmt.Sprintf("#%d", ev.Number)}, {text: ev.Grade.String(), color: gradeColor(ev.Grade)}, {text: k, color: labelColor(k)}, {text: ev.Detail}, {text: truncate(title), url: ev.URL}})
	}
	for _, d := range decisions {
		l := label(d.Action, apply)
		title := d.Title
		if bordered {
			title = compactTitle(title, d.Dependencies)
		}
		rows = append(rows, []cell{{text: fmt.Sprintf("#%d", d.Number)}, {text: d.Grade.String(), color: gradeColor(d.Grade)}, {text: l, color: labelColor(l)}, {text: d.Reason}, {text: truncate(title), url: d.URL}})
	}
	if bordered && len(rows) > 0 {
		values := make([][]string, 0, len(rows))
		for _, row := range rows {
			values = append(values, []string{row[0].text, row[1].text, statusLabel(row[2].text), row[3].text, row[4].text})
		}
		fmt.Fprintln(w, formatTable(w, s, []string{"PR", "UPDATE", "STATUS", "REASON", "TITLE"}, values))
	} else {
		table(w, s, "  ", rows)
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "  %s %s\n", s.paint(yellow, "warning:"), s.paint("", warning))
	}

	if res.Error == "" && len(res.Findings) > 0 {
		fmt.Fprintln(w, "  "+s.paint(bold+";"+yellow, "needs attention:"))
		findings := res.Findings
		if bordered {
			findings = tableFindings(res)
		}
		for _, f := range findings {
			suffix := ""
			if bordered && len(f.Suggestions) > 0 {
				suffix = " — " + s.paint(cyan, "suggest:")
			}
			fmt.Fprintf(w, "    %s %s%s\n", s.paint(yellow, "-"), s.paint("", f.Summary), suffix)
			if listsPRs(f.Kind) {
				fmt.Fprintf(w, "      %s %s\n", s.paint(dim, "PRs:"), prList(f.PRs))
			}
			for _, d := range f.Details {
				fmt.Fprintf(w, "      %s\n", s.paint(dim, "- "+d))
			}
			for _, sg := range f.Suggestions {
				if bordered {
					fmt.Fprintf(w, "      %s\n", s.paint("", invocationCommand(sg)))
				} else {
					fmt.Fprintf(w, "      %s %s\n", s.paint(cyan, "suggest:"), s.paint("", sg))
				}
			}
		}
	}
	fmt.Fprintln(w)
}

// listsPRs reports whether a finding's PRs are shown after it: the others
// name theirs in their summary.
func listsPRs(k policy.FindingKind) bool {
	switch k {
	case policy.FindingRepoCheck, policy.FindingNoChecks, policy.FindingReviewRequired, policy.FindingBehind:
		return true
	}
	return false
}

// HasPRs reports whether a repository had open Dependabot PRs to report,
// or failed before it could tell. One with a warning counts too: the
// warning may be the run's, such as a verdict cache found unreadable as it
// started, and must not be hidden with the repository. Findings count too:
// a truncated search can leave unread PRs even when every returned hit
// has closed since it was indexed.
func HasPRs(res runner.RepoResult) bool {
	return res.Error != "" || len(res.Decisions) > 0 || len(res.Events) > 0 || len(res.Warnings) > 0 || len(res.Findings) > 0
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
// Supplying elapsed adds the table-only payoff; omit it for legacy list output.
func Summary(w io.Writer, apply bool, results []runner.RepoResult, hidden int, elapsed ...time.Duration) error {
	out := &errorWriter{w: w}
	summary(out, apply, results, hidden, styleFor(w), elapsed...)
	return out.err
}

func summary(w io.Writer, apply bool, results []runner.RepoResult, hidden int, s style, elapsed ...time.Duration) {
	counts := map[string]int{}
	// applicable counts what --apply would do: not what a repository in
	// report mode would, as it is never changed.
	applicable := map[string]int{}
	findings, failed := 0, 0
	for _, r := range results {
		// Failed as the exit status counts it: a failed write or review
		// fails the run as much as a repository that could not be read.
		if r.Failed() {
			failed++
		}
		for _, ev := range r.Events {
			counts[string(ev.Kind)]++
		}
		if r.Error != "" {
			// As the report shows it: the writes made, not the decisions
			// and findings of a read the failure may have left stale.
			continue
		}
		findings += len(r.Findings)
		for _, d := range pending(r) {
			counts[label(d.Action, applies(apply, r))]++
			if applies(true, r) {
				applicable[label(d.Action, apply)]++
			}
		}
	}
	if len(elapsed) > 0 {
		defer func() {
			line := "done in " + max(0, elapsed[0]).Round(time.Second).String()
			if !apply {
				line = "elapsed " + max(0, elapsed[0]).Round(time.Second).String()
			} else if findings > 0 {
				verb := "need"
				if findings == 1 {
					verb = "needs"
				}
				line += fmt.Sprintf(" · %d %s you: %s", findings, verb, invocationCommand("deps-handler dependabot --open"))
			}
			fmt.Fprintln(w, s.paint(dim, line))
		}()
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
		if applicable["would merge"] > 0 {
			todo = append(todo, "merge")
		}
		if applicable["would rebase"] > 0 {
			todo = append(todo, "rebase")
		}
		if len(todo) > 0 {
			fmt.Fprintln(w, s.paint(dim, "dry run: nothing was changed; rerun with --apply to "+strings.Join(todo, " and ")))
		}
	} else if counts["would merge"] > 0 || counts["would rebase"] > 0 {
		fmt.Fprintln(w, s.paint(dim, "repos in report mode (mode: report) were only reported on"))
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

// applies reports whether a run that applies, or not, acts on res's
// repository: one in report mode is only reported on.
func applies(apply bool, res runner.RepoResult) bool {
	return apply && res.Policy.Mode != config.ModeReport
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

// truncate shortens a title to titleWidth characters, counting only those
// the report shows: invisible ones cannot push the title out of view.
func truncate(s string) string {
	s = Plain(s)
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

// invocationCommand recognizes the installed dh executable/symlink. A shell
// alias expanded to deps-handler is indistinguishable from its long name.
func invocationCommand(command string) string {
	if len(os.Args) > 0 && strings.EqualFold(strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe"), "dh") && strings.HasPrefix(command, "deps-handler ") {
		return "dh " + strings.TrimPrefix(command, "deps-handler ")
	}
	return command
}

// tableFindings groups presentation copies only; scripting formats and the
// underlying finding count retain every observation. Individual failures and
// agent assessments always keep their own context.
func tableFindings(res runner.RepoResult) []policy.Finding {
	var out []policy.Finding
	keys := map[string]int{}
	members := map[int][]string{}
	for _, f := range res.Findings {
		if len(f.Details) > 0 || f.Kind == policy.FindingIncompatible || f.Kind == policy.FindingRepoCheck || f.Kind == policy.FindingAgentRejected || f.Kind == policy.FindingAgentFailed || len(f.PRs) != 1 {
			out = append(out, f)
			continue
		}
		pr := fmt.Sprintf("#%d", f.PRs[0])
		message, member := f.Summary, pr
		kind := string(f.Kind)
		// Recognize the policy's exact stock messages so custom details are
		// never discarded. Minor and major holds share the same remedy.
		grade := ""
		switch f.Kind {
		case policy.FindingHeldMinor:
			grade = "minor"
		case policy.FindingHeldMajor:
			grade = "major"
		}
		if grade != "" {
			review := ""
			if res.Policy.AgentReview {
				review = ", or run with --review"
			}
			stock := fmt.Sprintf("%s is a %s update held only by policy (allow with max_update_type: %s%s)", pr, grade, grade, review)
			if message == stock {
				message = "held by policy: {PRs} — allow with max_update_type" + review
				member += " (" + grade + ")"
				kind = "held-policy"
			}
		}
		if strings.HasPrefix(message, pr+" ") || strings.HasPrefix(message, pr+":") {
			message = "{PRs}" + strings.TrimPrefix(message, pr)
		}
		if !strings.Contains(message, "{PRs}") {
			out = append(out, f)
			continue
		}
		// Compare equivalent suggestions, but retain each distinct command
		// verbatim so grouping never redirects a command to another PR.
		normalized := make([]string, len(f.Suggestions))
		for i, suggestion := range f.Suggestions {
			normalized[i] = findingPR.ReplaceAllStringFunc(suggestion, func(match string) string {
				if match == pr {
					return "{PR}"
				}
				return match
			})
		}
		encoded, _ := json.Marshal(normalized)
		key := kind + "\x00" + message + "\x00" + string(encoded)
		if i, ok := keys[key]; ok {
			members[i] = append(members[i], member)
			out[i].PRs = append(out[i].PRs, f.PRs...)
			for _, suggestion := range f.Suggestions {
				if !slices.Contains(out[i].Suggestions, suggestion) {
					out[i].Suggestions = append(out[i].Suggestions, suggestion)
				}
			}
			out[i].Summary = strings.ReplaceAll(message, "{PRs}", strings.Join(members[i], ", "))
		} else {
			i := len(out)
			keys[key] = i
			members[i] = []string{member}
			f.PRs = slices.Clone(f.PRs)
			f.Suggestions = slices.Clone(f.Suggestions)
			f.Summary = strings.ReplaceAll(message, "{PRs}", member)
			out = append(out, f)
		}
	}
	return out
}

// compactTitle only uses explicit metadata; titles are never parsed for
// versions. A named group remains a group even when it has just one entry.
func compactTitle(title string, deps []dependabot.Dependency) string {
	if len(deps) != 1 {
		return title
	}
	d := deps[0]
	if d.Group != "" || strings.TrimSpace(Plain(d.Name)) == "" || strings.TrimSpace(Plain(d.PreviousVersion)) == "" || strings.TrimSpace(Plain(d.Version)) == "" {
		return title
	}
	return d.Name + " " + d.PreviousVersion + " → " + d.Version
}

var findingPR = regexp.MustCompile(`#[0-9]+\b`)
