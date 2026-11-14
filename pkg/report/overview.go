package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

// Overview is the compact dry-run summary. It shares pending-decision and
// report-mode handling with Summary; each PR belongs to exactly one bucket.
func Overview(w io.Writer, results []runner.RepoResult, hidden int, next string) error {
	out := &errorWriter{w: w}
	var total overviewCounts
	failed := 0
	for _, r := range results {
		counts := countOverview(r)
		total.merge += counts.merge
		total.wait += counts.wait
		total.held += counts.held
		total.fail += counts.fail
		line := r.Repo + ": " + counts.String()
		if r.Error != "" {
			failed++
			line = r.Repo + ": error: " + r.Error
		}
		if len(r.Warnings) > 0 {
			line += "; warning: " + strings.Join(r.Warnings, "; ")
		}
		if r.Error == "" && len(r.Findings) > 0 {
			verb := "need"
			if len(r.Findings) == 1 {
				verb = "needs"
			}
			line += fmt.Sprintf("; %d %s attention", len(r.Findings), verb)
		}
		fmt.Fprintln(out, Plain(line))
	}
	if len(results) == 0 {
		fmt.Fprintln(out, Plain("no open Dependabot PRs in "+plural(hidden, "repo")))
	}
	line := "total: " + total.String()
	if failed > 0 {
		line += "; " + plural(failed, "repo") + " failed"
	}
	fmt.Fprintln(out, Plain(line))
	flag := " --open"
	if total.merge > 0 {
		flag = " --apply"
	}
	fmt.Fprintln(out, Plain("next: "+invocationCommand(next+flag)))
	return out.err
}

type overviewCounts struct{ merge, wait, held, fail int }

func (c overviewCounts) String() string {
	return fmt.Sprintf("%d would merge / %d waiting / %d held / %d failing", c.merge, c.wait, c.held, c.fail)
}

func countOverview(r runner.RepoResult) overviewCounts {
	var c overviewCounts
	// Never count decisions left stale by a failed read.
	if r.Error != "" {
		return c
	}
	for _, d := range pending(r) {
		switch {
		case len(d.FailedChecks) > 0:
			c.fail++
		case d.Action == policy.ActionMerge && applies(true, r):
			c.merge++
		case d.Action == policy.ActionWait || d.Action == policy.ActionRebase && applies(true, r):
			c.wait++
		default:
			c.held++
		}
	}
	return c
}
