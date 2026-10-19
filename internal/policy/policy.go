// Package policy decides, without side effects, what deps-merger should do
// with each Dependabot pull request and what the repository needs fixing.
package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/dependabot"
	"github.com/sglavoie/deps-merger/internal/github"
)

// Action is what deps-merger does with a pull request.
type Action string

const (
	ActionMerge  Action = "merge"  // safe: merge now
	ActionRebase Action = "rebase" // safe but behind: ask Dependabot to rebase
	ActionWait   Action = "wait"   // transient state: try again next run
	ActionSkip   Action = "skip"   // not safe: leave untouched
)

// RebaseCommand is the comment that asks Dependabot to rebase a PR.
const RebaseCommand = "@dependabot rebase"

// Decision is the verdict for one pull request.
type Decision struct {
	PR           *github.PullRequest     `json:"-"`
	Number       int                     `json:"number"`
	Title        string                  `json:"title"`
	URL          string                  `json:"url"`
	Grade        config.UpdateType       `json:"grade"`
	Dependencies []dependabot.Dependency `json:"dependencies"`
	Action       Action                  `json:"action"`
	Reason       string                  `json:"reason"`
	FailedChecks []string                `json:"failed_checks,omitempty"`
}

// Evaluate decides what to do with every Dependabot PR of repo. viewer is
// the authenticated login, used to avoid repeating our own rebase requests.
func Evaluate(repo *github.Repo, p config.Policy, viewer string) []Decision {
	out := make([]Decision, 0, len(repo.PullRequests))
	for i := range repo.PullRequests {
		pr := &repo.PullRequests[i]
		if !dependabot.IsLogin(pr.Author) {
			continue // never touch PRs Dependabot did not open
		}
		d := evaluate(repo, pr, p, viewer)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func evaluate(repo *github.Repo, pr *github.PullRequest, p config.Policy, viewer string) Decision {
	d := Decision{PR: pr, Number: pr.Number, Title: pr.Title, URL: pr.URL, Dependencies: []dependabot.Dependency{}}
	for _, c := range pr.Commits {
		d.Dependencies = append(d.Dependencies, dependabot.ParseCommitMessage(c.Message)...)
	}
	d.Grade = dependabot.Grade(d.Dependencies)
	seen := map[string]bool{}
	for _, c := range pr.Checks {
		if c.State == github.CheckFailed && !seen[c.Name] {
			seen[c.Name] = true // matrix jobs can repeat a name
			d.FailedChecks = append(d.FailedChecks, c.Name)
		}
	}

	skip := func(format string, args ...any) Decision {
		d.Action, d.Reason = ActionSkip, fmt.Sprintf(format, args...)
		return d
	}

	if repo.Archived {
		return skip("repository is archived")
	}
	if pr.IsDraft {
		return skip("draft pull request")
	}
	if pr.CommitCount > len(pr.Commits) {
		return skip("too many commits to verify (%d)", pr.CommitCount)
	}
	for _, c := range pr.Commits {
		if c.AuthorCount > len(c.Authors) {
			return skip("too many authors to verify on commit %.7s (%d)", c.OID, c.AuthorCount)
		}
		for _, a := range c.Authors {
			if !dependabot.IsCommitAuthor(a.Login, a.Name, a.Email) {
				who := a.Login
				if who == "" {
					who = a.Name
				}
				return skip("modified by %s (commit %.7s); left for a human", who, c.OID)
			}
		}
		// Author identities can be forged; GitHub's signature cannot.
		if !c.SignedByGitHub {
			return skip("commit %.7s is not signed by GitHub; left for a human", c.OID)
		}
	}
	if d.Grade == config.UpdateUnknown {
		return skip("cannot determine update type from Dependabot metadata")
	}
	if d.Grade > p.MaxUpdateType {
		return skip("%s update exceeds limit (%s)", d.Grade, p.MaxUpdateType)
	}

	if pr.CheckCount > len(pr.Checks) {
		return skip("too many checks to verify (%d)", pr.CheckCount)
	}
	pending := 0
	for _, c := range pr.Checks {
		if c.State == github.CheckPending {
			pending++
		}
	}
	switch {
	case len(d.FailedChecks) > 0:
		return skip("failing checks: %s", strings.Join(d.FailedChecks, ", "))
	case pending > 0:
		d.Action, d.Reason = ActionWait, fmt.Sprintf("%d check(s) still running", pending)
		return d
	case len(pr.Checks) == 0 && p.RequireChecks:
		return skip("no checks ran (set require_checks: false to opt in)")
	}

	switch pr.Mergeable {
	case "CONFLICTING":
		return skip("merge conflicts (Dependabot normally rebases; try @dependabot recreate)")
	case "UNKNOWN", "":
		d.Action, d.Reason = ActionWait, "GitHub is still computing mergeability"
		return d
	}

	var safe Decision
	switch pr.MergeState {
	case "CLEAN", "HAS_HOOKS":
		safe = d
		safe.Action, safe.Reason = ActionMerge, "checks green, within policy"
	case "BEHIND":
		if rebaseRequested(pr, viewer) {
			d.Action, d.Reason = ActionWait, "rebase already requested"
			return d
		}
		safe = d
		safe.Action, safe.Reason = ActionRebase, "behind base branch"
	case "BLOCKED":
		if pr.ReviewDecision == "REVIEW_REQUIRED" || pr.ReviewDecision == "CHANGES_REQUESTED" {
			return skip("branch protection requires an approving review")
		}
		return skip("blocked by branch protection")
	case "DIRTY":
		return skip("merge conflicts")
	case "UNKNOWN", "":
		d.Action, d.Reason = ActionWait, "GitHub is still computing merge state"
		return d
	default:
		return skip("merge state %s", pr.MergeState)
	}

	if !repo.CanWrite() {
		safe.Action, safe.Reason = ActionSkip, fmt.Sprintf("would %s, but you only have %s access", safe.Action, strings.ToLower(repo.Permission))
	}
	return safe
}

// rebaseRequested reports whether viewer already asked for a rebase since
// the PR's latest commit.
func rebaseRequested(pr *github.PullRequest, viewer string) bool {
	if len(pr.Commits) == 0 {
		return false
	}
	last := pr.Commits[len(pr.Commits)-1].CommittedDate
	for _, c := range pr.Comments {
		if c.Author == viewer && c.CreatedAt.After(last) && strings.Contains(c.Body, RebaseCommand) {
			return true
		}
	}
	return false
}

// MergeMethod picks the configured method if the repo allows it, otherwise
// the first allowed one in squash, merge, rebase order.
func MergeMethod(repo *github.Repo, preferred string) (string, error) {
	allowed := map[string]bool{"squash": repo.SquashAllowed, "merge": repo.MergeAllowed, "rebase": repo.RebaseAllowed}
	if allowed[preferred] {
		return preferred, nil
	}
	for _, m := range []string{"squash", "merge", "rebase"} {
		if allowed[m] {
			return m, nil
		}
	}
	return "", fmt.Errorf("%s allows no merge method", repo.FullName)
}
