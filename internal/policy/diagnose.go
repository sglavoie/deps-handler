package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
)

// FindingKind classifies something in the repository worth fixing.
type FindingKind string

const (
	// FindingRepoCheck is a check that fails on every Dependabot PR that ran
	// it: the repository is broken, not the updates.
	FindingRepoCheck FindingKind = "repo-wide-failure"
	// FindingIncompatible is an update that fails its own checks.
	FindingIncompatible FindingKind = "incompatible-update"
	// FindingHeldMajor is a green major update held back by policy.
	FindingHeldMajor FindingKind = "held-major"
	// FindingNoChecks means the repository runs no CI on Dependabot PRs.
	FindingNoChecks FindingKind = "no-checks"
	// FindingReadOnly means the viewer cannot merge in this repository.
	FindingReadOnly FindingKind = "read-only"
	// FindingTruncated means GitHub has more open Dependabot PRs than one
	// fetch returns, so some were not evaluated.
	FindingTruncated FindingKind = "truncated"
)

// Finding is a repository-level observation with suggested fixes. Fixes
// are never applied automatically.
type Finding struct {
	Kind        FindingKind `json:"kind"`
	Summary     string      `json:"summary"`
	PRs         []int       `json:"prs,omitempty"`
	Suggestions []string    `json:"suggestions,omitempty"`
}

// Diagnose looks across all decisions of one repository for problems that
// need a change in the repository rather than a merge.
func Diagnose(repo *github.Repo, p config.Policy, decisions []Decision) []Finding {
	out := []Finding{}
	if repo.OpenPRCount > len(repo.PullRequests) {
		out = append(out, Finding{
			Kind:    FindingTruncated,
			Summary: fmt.Sprintf("only %d of %d open Dependabot PRs were evaluated; the rest wait for a later run", len(repo.PullRequests), repo.OpenPRCount),
		})
	}
	if len(decisions) == 0 {
		return out
	}

	if !repo.CanWrite() {
		out = append(out, Finding{
			Kind:    FindingReadOnly,
			Summary: fmt.Sprintf("you have %s access; deps-merger can only report here", strings.ToLower(repo.Permission)),
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

	noChecks := []int{}
	for _, d := range decisions {
		own := ownFailures(d, repoWide)
		switch {
		case len(own) > 0:
			out = append(out, Finding{
				Kind:        FindingIncompatible,
				Summary:     fmt.Sprintf("#%d (%s) fails %s", d.Number, d.Grade, strings.Join(own, ", ")),
				PRs:         []int{d.Number},
				Suggestions: ignoreSuggestions(d),
			})
		case d.Grade == config.UpdateMajor && p.MaxUpdateType < config.UpdateMajor &&
			len(d.PR.Checks) > 0 && allPassed(d.PR.Checks):
			out = append(out, Finding{
				Kind:    FindingHeldMajor,
				Summary: fmt.Sprintf("#%d is a green major update held by policy (allow with max_update_type: major)", d.Number),
				PRs:     []int{d.Number},
			})
		}
		if len(d.PR.Checks) == 0 {
			noChecks = append(noChecks, d.Number)
		}
	}
	if len(noChecks) > 0 && p.RequireChecks {
		out = append(out, Finding{
			Kind:    FindingNoChecks,
			Summary: "no checks ran on these Dependabot PRs; add CI or opt in with require_checks: false",
			PRs:     noChecks,
		})
	}
	return out
}

// repoWideChecks returns check names that failed on every PR that ran them,
// on at least two PRs, mapped to those PR numbers. The check must also have
// run on at least half of the PRs, so a job only stale PRs still carry is not
// mistaken for a broken base branch.
func repoWideChecks(decisions []Decision) map[string][]int {
	ran := map[string]int{}
	failed := map[string][]int{}
	for _, d := range decisions {
		seen := map[string]bool{}
		for _, c := range d.PR.Checks {
			if c.Name == "" || seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			ran[c.Name]++
			if c.State == github.CheckFailed {
				failed[c.Name] = append(failed[c.Name], d.Number)
			}
		}
	}
	out := map[string][]int{}
	for name, prs := range failed {
		if len(prs) >= 2 && len(prs) == ran[name] && 2*ran[name] >= len(decisions) {
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

func allPassed(checks []github.Check) bool {
	for _, c := range checks {
		if c.State != github.CheckPassed {
			return false
		}
	}
	return true
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
