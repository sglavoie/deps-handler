package policy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/github"
)

// FindingKind classifies something in the repository worth fixing.
type FindingKind string

const (
	// FindingRepoCheck is a check that fails on every Dependabot PR that ran
	// it: the repository is broken, not the updates.
	FindingRepoCheck FindingKind = "repo-wide-failure"
	// FindingIncompatible is an update that fails its own checks.
	FindingIncompatible FindingKind = "incompatible-update"
	// FindingHeldMajor is a major update held back by policy alone.
	FindingHeldMajor FindingKind = "held-major"
	// FindingHeldMinor is a minor update held back by policy alone, under
	// max_update_type: patch.
	FindingHeldMinor FindingKind = "held-minor"
	// FindingHeldUnknown is an update held back by policy alone because
	// Dependabot records no semver size for it.
	FindingHeldUnknown FindingKind = "held-unknown"
	// FindingNoChecks means the repository runs no CI on Dependabot PRs.
	FindingNoChecks FindingKind = "no-checks"
	// FindingArchived means the repository is archived, so none of its
	// Dependabot PRs can merge.
	FindingArchived FindingKind = "archived"
	// FindingReadOnly means the viewer cannot merge in this repository.
	FindingReadOnly FindingKind = "read-only"
	// FindingTruncated means GitHub has more open Dependabot PRs than one
	// fetch returns, so some were not evaluated.
	FindingTruncated FindingKind = "truncated"
	// FindingReviewRequired means branch protection wants an approving
	// review, which deps-merger never gives.
	FindingReviewRequired FindingKind = "review-required"
	// FindingStalledRebase is a rebase request Dependabot has not acted on.
	FindingStalledRebase FindingKind = "stalled-rebase"
	// FindingBehind is PRs that need a rebase deps-merger may not ask
	// for: dependabot_commands is off.
	FindingBehind FindingKind = "behind"
	// FindingAgentRejected is an update the AI agent judged unsafe.
	FindingAgentRejected FindingKind = "agent-rejected"
	// FindingAgentFailed is an update the AI agent could not review.
	FindingAgentFailed FindingKind = "agent-failed"
)

// Finding is a repository-level observation with suggested fixes. Fixes
// are never applied automatically.
type Finding struct {
	Kind        FindingKind `json:"kind"`
	Summary     string      `json:"summary"`
	PRs         []int       `json:"prs"`
	Details     []string    `json:"details"`
	Suggestions []string    `json:"suggestions"`
}

// MarshalJSON writes every list, empty ones as [] rather than null, so a
// script can read a finding's fields without checking for them first.
func (f Finding) MarshalJSON() ([]byte, error) {
	type plain Finding
	if f.PRs == nil {
		f.PRs = []int{}
	}
	if f.Details == nil {
		f.Details = []string{}
	}
	if f.Suggestions == nil {
		f.Suggestions = []string{}
	}
	return json.Marshal(plain(f))
}

// Diagnose looks across all decisions of one repository for problems that
// need a change in the repository rather than a merge.
func Diagnose(repo *github.Repo, p config.Policy, decisions []Decision) []Finding {
	out := []Finding{}
	// The search counts closed PRs its index has not caught up with, which
	// are dropped from PullRequests: only results it did not return were
	// left unread.
	if repo.OpenPRCount > repo.SearchCount {
		out = append(out, Finding{
			Kind:    FindingTruncated,
			Summary: fmt.Sprintf("only the oldest %d of %d open Dependabot PRs were evaluated; newer ones wait until older ones close", repo.SearchCount, repo.OpenPRCount),
		})
	}
	if len(decisions) == 0 {
		return out
	}

	if repo.Archived {
		out = append(out, Finding{
			Kind:        FindingArchived,
			Summary:     "the repository is archived; its Dependabot PRs cannot merge",
			Suggestions: []string{"set skip: true on it in the config, or unarchive it"},
		})
	}
	if !repo.CanWrite() {
		out = append(out, Finding{
			Kind:    FindingReadOnly,
			Summary: lacksWrite(repo) + "; deps-merger can only report here",
		})
	}

	repoWide := repoWideChecks(decisions)
	for _, name := range sortedKeys(repoWide) {
		prs := repoWide[name]
		all := fmt.Sprintf("all %d", len(prs))
		if len(prs) == 2 {
			all = "both"
		}
		out = append(out, Finding{
			Kind:    FindingRepoCheck,
			Summary: fmt.Sprintf("check %q fails on %s Dependabot PRs that ran it; fix it on the base branch", name, all),
			PRs:     prs,
		})
	}

	noChecks, needsReview, behind := []int{}, []int{}, []int{}
	for _, d := range decisions {
		own := ownFailures(d, repoWide)
		switch {
		case !d.assessed:
			// A PR someone else changed, or whose checks ran on another
			// commit, says nothing about whether the update itself works.
		case len(own) > 0:
			out = append(out, Finding{
				Kind:        FindingIncompatible,
				Summary:     fmt.Sprintf("#%d (%s) fails %s", d.Number, d.Grade, strings.Join(own, ", ")),
				PRs:         []int{d.Number},
				Suggestions: ignoreSuggestions(d),
			})
		case d.Verdict != nil && d.Verdict.Error != "":
			out = append(out, Finding{
				Kind:    FindingAgentFailed,
				Summary: fmt.Sprintf("#%d (%s): the agent could not review it: %s", d.Number, d.Grade, d.Verdict.Error),
				PRs:     []int{d.Number},
			})
		case d.Verdict != nil && !d.Verdict.Approved:
			out = append(out, Finding{
				Kind:    FindingAgentRejected,
				Summary: fmt.Sprintf("#%d (%s) rejected by the agent: %s", d.Number, d.Grade, d.Verdict.Summary),
				PRs:     []int{d.Number},
				Details: d.Verdict.Concerns,
			})
		case d.heldForSize && d.Grade == config.UpdateUnknown:
			out = append(out, Finding{
				Kind:    FindingHeldUnknown,
				Summary: fmt.Sprintf("#%d is held only because Dependabot records no semver size for it (merge it by hand%s)", d.Number, orReview(p)),
				PRs:     []int{d.Number},
			})
		case d.heldForSize:
			kind := FindingHeldMajor
			if d.Grade == config.UpdateMinor {
				kind = FindingHeldMinor
			}
			out = append(out, Finding{
				Kind:    kind,
				Summary: fmt.Sprintf("#%d is a %s update held only by policy (allow with max_update_type: %s%s)", d.Number, d.Grade, d.Grade, orReview(p)),
				PRs:     []int{d.Number},
			})
		}
		// A PR still waiting may yet get checks, and one that is a draft,
		// someone else's work or in an archived repository was never
		// judged on its checks at all. Checks that were all skipped
		// tested nothing either. A PR skipped for its size is not left to
		// wait, so its age says whether CI has had time to start.
		young := repo.FetchedAt.Sub(pushedAt(d.PR)) < checksGrace
		if d.assessed && !anyRan(LatestChecks(d.PR.Checks)) && d.Action != ActionWait && !young && !inConflict(d.PR) {
			noChecks = append(noChecks, d.Number)
		}
		if d.needsReview {
			needsReview = append(needsReview, d.Number)
		}
		if d.behind {
			behind = append(behind, d.Number)
		}
		if d.stalledRebase {
			out = append(out, Finding{
				Kind:        FindingStalledRebase,
				Summary:     fmt.Sprintf("#%d: Dependabot has not acted on the rebase request; the next PR is rebased instead", d.Number),
				PRs:         []int{d.Number},
				Suggestions: []string{"@dependabot recreate"},
			})
		}
	}
	if len(needsReview) > 0 {
		out = append(out, Finding{
			Kind:        FindingReviewRequired,
			Summary:     "branch protection requires an approving review, which deps-merger never gives",
			PRs:         needsReview,
			Suggestions: []string{"approve these PRs, or let Dependabot PRs merge without review"},
		})
	}
	if len(behind) > 0 {
		out = append(out, Finding{
			Kind:        FindingBehind,
			Summary:     "behind the base branch, which must be up to date to merge, and deps-merger posts no Dependabot commands here (dependabot_commands: false)",
			PRs:         behind,
			Suggestions: []string{fmt.Sprintf("comment %s on one at a time: each merge puts the others behind again", RebaseCommand)},
		})
	}
	if len(noChecks) > 0 && p.RequireChecks && p.MergeWithoutChecksAfter == 0 {
		out = append(out, Finding{
			Kind:    FindingNoChecks,
			Summary: "no checks ran on these Dependabot PRs; add CI or opt in with require_checks: false or merge_without_checks_after",
			PRs:     noChecks,
		})
	}
	return out
}

// repoWideChecks returns check names that failed on every PR that ran them,
// on at least two PRs, mapped to those PR numbers. The check must also have
// run on at least half of the PRs, so a job only stale PRs still carry is not
// mistaken for a broken base branch. Only PRs judged on Dependabot's own head
// commit count, for both: a check failing where a human pushed, or on an
// older commit, says nothing about the base branch, and such PRs must not
// dilute the half either.
func repoWideChecks(decisions []Decision) map[string][]int {
	ran := map[string]int{}
	failed := map[string][]int{}
	judged := 0
	for _, d := range decisions {
		if !d.assessed {
			continue
		}
		judged++
		seen := map[string]bool{}
		for _, c := range LatestChecks(d.PR.Checks) {
			// Only a passed or failed job counts: a skipped one did not run
			// here, and one pending or stale has no say yet. Counting those
			// would hide a broken base branch behind a job still running,
			// and suggest ignoring every update that failed it.
			if c.Name == "" || seen[c.Name] || (c.State != github.CheckPassed && c.State != github.CheckFailed) {
				continue
			}
			seen[c.Name] = true
			ran[c.Name]++
		}
		for _, name := range d.FailedChecks {
			failed[name] = append(failed[name], d.Number)
		}
	}
	out := map[string][]int{}
	for name, prs := range failed {
		if len(prs) >= 2 && len(prs) == ran[name] && 2*ran[name] >= judged {
			out[name] = prs
		}
	}
	return out
}

func ownFailures(d Decision, repoWide map[string][]int) []string {
	var own []string
	for _, name := range d.FailedChecks {
		if _, ok := repoWide[name]; !ok {
			own = append(own, name)
		}
	}
	return own
}

// orReview suggests an agent review for a held update, where the repo
// allows one: a PR held for its size alone was not reviewed this run.
func orReview(p config.Policy) string {
	if p.AgentReview {
		return ", or run with --review"
	}
	return ""
}

const maxSuggestions = 5

// ignoreSuggestions returns Dependabot comment commands that would stop the
// failing update from coming back.
func ignoreSuggestions(d Decision) []string {
	grouped := len(d.Dependencies) > 1
	for _, dep := range d.Dependencies {
		if dep.Group != "" {
			grouped = true
		}
	}
	if !grouped {
		switch d.Grade {
		case config.UpdateMajor:
			return []string{"@dependabot ignore this major version"}
		case config.UpdateMinor:
			return []string{"@dependabot ignore this minor version"}
		case config.UpdatePatch:
			return []string{"@dependabot ignore this patch version"}
		}
		return nil
	}
	// In a group the culprit is unknown; the riskiest bumps are the likeliest.
	var out []string
	for _, dep := range d.Dependencies {
		if dep.UpdateType == d.Grade && (d.Grade == config.UpdateMajor || d.Grade == config.UpdateMinor) {
			out = append(out, fmt.Sprintf("@dependabot ignore %s %s version", dep.Name, d.Grade))
		}
	}
	if len(out) > maxSuggestions {
		out = append(out[:maxSuggestions], fmt.Sprintf("(and %d more %s bumps in this group)", len(out)-maxSuggestions, d.Grade))
	}
	return out
}

func sortedKeys(m map[string][]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
