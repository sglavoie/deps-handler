package pr

import (
	"fmt"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/policy"
)

// BaseComparison preserves observations separately from the agent's causal
// interpretation. HeadOID identifies the CI being compared, including when a
// later repair updates Result.Snapshot to a different commit.
type BaseComparison struct {
	HeadOID string            `json:"head_oid"`
	Base    github.BaseChecks `json:"base"`
	Checks  []CheckComparison `json:"checks"`
	Gaps    []string          `json:"gaps"`
}

type CheckComparison struct {
	Head    github.PRCheck   `json:"head"`
	Base    []github.PRCheck `json:"base"`
	Outcome string           `json:"outcome"`
	Note    string           `json:"note"`
}

func latestPRChecks(checks []github.PRCheck) []github.PRCheck {
	plain := make([]github.Check, 0, len(checks))
	for _, c := range checks {
		plain = append(plain, c.Check)
	}
	current := make(map[github.Check]bool)
	for _, c := range policy.LatestChecks(plain) {
		current[c] = true
	}
	out := make([]github.PRCheck, 0, len(checks))
	for _, c := range checks {
		if current[c.Check] {
			out = append(out, c)
		}
	}
	return out
}

func compareBase(p github.PRSnapshot, base github.BaseChecks) *BaseComparison {
	base.Checks = latestPRChecks(base.Checks)
	c := &BaseComparison{HeadOID: p.HeadOID, Base: base, Checks: []CheckComparison{}, Gaps: []string{}}
	if base.OID == "" || base.OID != p.BaseOID {
		c.Gaps = append(c.Gaps, "base evidence does not identify the recorded base commit")
	}
	if base.EvidenceError != "" {
		c.Gaps = append(c.Gaps, base.EvidenceError)
	}
	if !base.ChecksComplete {
		c.Gaps = append(c.Gaps, "base check list is incomplete; missing checks and superseding runs may be unread")
	}
	if !p.ChecksComplete {
		c.Gaps = append(c.Gaps, "PR check list is incomplete")
	}
	for _, check := range base.Checks {
		if check.EvidenceError != "" {
			c.Gaps = append(c.Gaps, "Base evidence gap for "+checkLabel(check)+": "+check.EvidenceError)
		}
	}
	for _, head := range latestPRChecks(p.Checks) {
		row := CheckComparison{Head: head, Base: []github.PRCheck{}, Outcome: "inconclusive"}
		// Status contexts have names but no producer identity in the rollup.
		// Likewise, an external check run cannot be assigned to a workflow.
		identified := !head.Status && head.Name != "" && head.App > 0 && head.Workflow > 0 && head.Event != ""
		for _, b := range base.Checks {
			if b.Name == head.Name && b.Status == head.Status && b.App == head.App && b.Workflow == head.Workflow {
				b.Log = "" // Full excerpts live once in Base.Checks.
				row.Base = append(row.Base, b)
			}
		}
		// Prefer the same trigger. A push run remains useful evidence when
		// no pull_request run exists, but differences are always explicit.
		sameEvent := []github.PRCheck{}
		for _, b := range row.Base {
			if b.Event == head.Event {
				sameEvent = append(sameEvent, b)
			}
		}
		if len(sameEvent) > 0 {
			row.Base = sameEvent
		}
		switch {
		case base.OID == "" || base.OID != p.BaseOID || base.EvidenceError != "" || !base.ChecksComplete || !p.ChecksComplete:
			row.Note = "CI evidence is unavailable, unpinned or incomplete; no reliable comparison is possible"
		case !identified:
			row.Outcome, row.Note = "uncomparable", "producer, workflow or trigger identity is missing; a shared check name alone cannot establish comparability"
		case len(row.Base) == 0:
			row.Outcome, row.Note = "missing-base-check", "no matching app, workflow and job was reported at the recorded base commit"
		case len(row.Base) > 1:
			row.Note = "multiple base runs match; their outcomes cannot be reduced to one result"
		case row.Base[0].Event == "":
			row.Note = "base trigger identity is missing"
		default:
			b := row.Base[0]
			switch {
			case head.State == github.CheckFailed && b.State == github.CheckFailed:
				row.Outcome, row.Note = "both-failed", "matching job failed on both commits; compare logs before claiming the root causes are the same"
			case head.State == github.CheckFailed && b.State == github.CheckPassed:
				row.Outcome, row.Note = "base-passed-pr-failed", "matching job passed on base and failed on the PR; this does not establish that the dependency update caused the failure"
			case (head.State == github.CheckPassed || head.State == github.CheckFailed) && (b.State == github.CheckPassed || b.State == github.CheckFailed):
				row.Outcome, row.Note = "different-outcomes", "matching jobs have different observed outcomes"
				if head.State == b.State {
					row.Outcome, row.Note = "same-outcome", "matching jobs have the same observed outcome"
				}
			default:
				row.Note = "pending, skipped, stale or unknown checks do not establish a passing or failing baseline"
			}
			if b.Event != head.Event {
				row.Note += fmt.Sprintf("; triggers differ (base %s, PR %s), so inputs, permissions and conditions may differ", b.Event, head.Event)
			}
		}
		c.Checks = append(c.Checks, row)
	}
	return c
}

func checkLabel(c github.PRCheck) string {
	if c.Status {
		return c.Name + " (status context)"
	}
	return fmt.Sprintf("%s (app %d, workflow %d, event %s)", c.Name, c.App, c.Workflow, c.Event)
}

func comparisonMarkdown(c *BaseComparison) string {
	text := fmt.Sprintf("## Base CI comparison\n\nPR head: `%s`\nBase commit: `%s`\n\nThese are observed job outcomes; matching failures do not prove matching root causes.\n\n", c.HeadOID, c.Base.OID)
	if len(c.Base.Checks) == 0 {
		text += "No base checks reported.\n\n"
	}
	for _, b := range c.Base.Checks {
		text += fmt.Sprintf("- Base %s: %s", checkLabel(b), b.State)
		if b.URL != "" {
			text += " (" + b.URL + ")"
		}
		text += "\n"
	}
	text += "\n"
	for _, row := range c.Checks {
		text += fmt.Sprintf("- %s: **%s**. %s.\n", checkLabel(row.Head), row.Outcome, row.Note)
		for _, b := range row.Base {
			text += fmt.Sprintf("  - Base %s: %s; PR: %s.\n", checkLabel(b), b.State, row.Head.State)
		}
	}
	text += "\n"
	for _, gap := range c.Gaps {
		text += "Evidence gap: " + gap + "\n\n"
	}
	for _, b := range c.Base.Checks {
		if b.Log != "" {
			text += "Base failure excerpt for " + checkLabel(b) + ":\n\n"
			// Indented code keeps even logs containing Markdown fences inert.
			text += "    " + strings.ReplaceAll(strings.TrimRight(b.Log, "\n"), "\n", "\n    ") + "\n\n"
		}
	}
	return text
}
