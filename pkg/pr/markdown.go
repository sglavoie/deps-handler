package pr

import (
	"fmt"
	"io"

	"github.com/sglavoie/deps-merger/pkg/review"
)

// Markdown writes a standalone handoff with the precise PR and commit.
func Markdown(w io.Writer, r Result) error {
	// Build before writing so the writer's first error is preserved.
	text := fmt.Sprintf("# %s: %s\n\nPR: %s\nHead: `%s`\nBase: `%s` (`%s`)\n\n", r.Snapshot.Target, review.Clean(r.Snapshot.Title), r.Snapshot.URL, r.Snapshot.HeadOID, r.Snapshot.BaseRef, r.Snapshot.BaseOID)
	if r.PlanSource != "" {
		text += "Repair plan: `" + r.PlanSource + "`\n\n"
	}
	if r.ChecksPassing {
		text += "Checks: passing at this snapshot.\n\n"
	} else {
		text += "Checks: not confirmed passing.\n\n"
	}
	text += "## Checks\n\n"
	if len(r.Snapshot.Checks) == 0 {
		text += "No checks reported.\n\n"
	}
	for _, c := range r.Snapshot.Checks {
		text += fmt.Sprintf("- %s: %s", review.Clean(c.Name), c.State)
		if c.URL != "" {
			text += " (" + c.URL + ")"
		}
		text += "\n"
	}
	text += "\n"
	sections := []struct {
		title string
		items []string
	}{
		{"Merge blockers", r.Blockers}, {"Root causes", r.Analysis.Causes}, {"Repair steps", r.Analysis.Steps}, {"Validation", r.Analysis.Validation}, {"Remaining work / evidence gaps", r.Analysis.Remaining},
	}
	if r.Analysis.Summary != "" {
		text += "## Assessment\n\n" + r.Analysis.Summary + "\n\n"
	}
	for _, section := range sections {
		text += "## " + section.title + "\n\n"
		if len(section.items) == 0 {
			text += "None reported.\n"
		}
		for i, item := range section.items {
			text += fmt.Sprintf("%d. %s\n", i+1, item)
		}
		text += "\n"
	}
	if len(r.Commits) > 0 {
		text += "## Pushed commits\n\n"
		for _, oid := range r.Commits {
			text += "- `" + oid + "`\n"
		}
		text += "\nCI may still require reviews or other merge gates listed above.\n\n"
	}
	if r.Workspace != "" {
		text += "Repair checkout kept at `" + r.Workspace + "`.\n\n"
	}
	if r.Error != "" {
		text += "Operation stopped: " + r.Error + "\n"
	}
	_, err := io.WriteString(w, review.Clean(text))
	return err
}
