package pr

import (
	"fmt"
	"io"
	"strings"
)

// HandleReport presents actionable findings alongside each PR's final outcome.
func HandleReport(w io.Writer, results []HandleResult, apply, list bool) error {
	tables := reportTables{destination: w}
	var b strings.Builder
	if !apply {
		b.WriteString("Preview only. Use --apply to authorize repairs, CI reruns and merging.\n\n")
	}
	if len(results) == 0 {
		b.WriteString("No open PRs selected.\n")
	}
	if !list && len(results) > 0 {
		rows := [][]string{}
		for _, r := range results {
			rows = append(rows, []string{r.Target.String(), r.Status, r.Summary})
		}
		b.WriteString(tables.add([]string{"PR", "Outcome", "Reason"}, rows) + "\n\n")
	}
	for _, r := range results {
		if list {
			fmt.Fprintf(&b, "## %s — %s\n\n%s\n\n", r.Target, r.Status, r.Summary)
		} else if len(r.Blockers) != 0 || len(r.Reviews) != 0 || r.Workspace != "" {
			fmt.Fprintf(&b, "%s\n", r.Target)
		}
		for _, blocker := range r.Blockers {
			fmt.Fprintf(&b, "- %s\n", blocker)
		}
		if len(r.Reviews) > 0 {
			a := r.Reviews[len(r.Reviews)-1].Analysis
			for _, next := range a.Recommendations {
				fmt.Fprintf(&b, "- %s (%s): %s Next: %s\n", next.Subject, next.Action, next.Reason, next.NextStep)
			}
			for _, remaining := range a.Remaining {
				fmt.Fprintf(&b, "- %s\n", remaining)
			}
		}
		for _, repair := range r.Repairs {
			if repair.ValidationStatus != "" {
				fmt.Fprintf(&b, "- Repair validation status: %s\n", repair.ValidationStatus)
			}
			for _, validation := range repair.Validation {
				fmt.Fprintf(&b, "- Repair validation: %s\n", validation)
			}
		}
		for _, oid := range r.Commits {
			fmt.Fprintf(&b, "- Pushed repair: %s\n", oid)
		}
		for _, rerun := range r.Reruns {
			for _, run := range rerun.Runs {
				fmt.Fprintf(&b, "- CI run %d: requested=%t, attempt=%d, conclusion=%s\n", run.RunID, run.Requested, run.ObservedAttempt, run.Conclusion)
			}
		}
		if r.Workspace != "" {
			fmt.Fprintf(&b, "- Checkout kept at %s\n", r.Workspace)
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, tables.clean(b.String()))
	return err
}
