// Package policy decides, without side effects, what deps-merger should do
// with each Dependabot pull request and what the repository needs fixing.
package policy

import (
	"fmt"
	"sort"
	"strings"
	"time"

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
	// assessed is true when the PR is Dependabot's own work and its checks
	// ran on its head commit, so they speak for the update itself.
	assessed bool
	// inFlight is true for a PR waiting on CI or GitHub that should merge
	// once they finish, so other PRs must not be rebased ahead of it.
	inFlight bool
	// stalledRebase is true when our rebase request went unanswered.
	stalledRebase bool
	// needsReview is true when branch protection wants an approval.
	needsReview bool
}

const (
	// checksGrace is how long a new commit may go without any check before
	// it counts as having no CI: GitHub registers checks only once a
	// workflow starts.
	checksGrace = 30 * time.Minute
	// rebaseTimeout is how long a rebase request may go unanswered before
	// the PR stops holding the rebase queue.
	rebaseTimeout = 24 * time.Hour
)

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
	queueRebases(out)
	return out
}

// queueRebases keeps at most one PR moving towards the base branch at a
// time. When the base must be up to date, each merge puts every other PR
// behind again, so rebasing them all at once reruns CI on each of them for
// every merge. The rest wait behind the next merge or rebase.
func queueRebases(ds []Decision) {
	ahead := 0
	for _, d := range ds {
		if d.Action == ActionMerge || d.inFlight {
			ahead = d.Number
			break
		}
	}
	for i := range ds {
		if ds[i].Action != ActionRebase {
			continue
		}
		if ahead == 0 {
			ahead = ds[i].Number
			continue
		}
		ds[i].Action, ds[i].Reason = ActionWait, fmt.Sprintf("queued behind #%d", ahead)
	}
}

func evaluate(repo *github.Repo, pr *github.PullRequest, p config.Policy, viewer string) Decision {
	d := Decision{PR: pr, Number: pr.Number, Title: pr.Title, URL: pr.URL, Dependencies: []dependabot.Dependency{}}
	for _, c := range pr.Commits {
		d.Dependencies = append(d.Dependencies, dependabot.ParseCommitMessage(c.Message)...)
	}
	d.Grade = dependabot.Grade(d.Dependencies)
	seen := map[string]bool{}
	var stale []string
	for _, c := range pr.Checks {
		if seen[c.Name] {
			continue // matrix jobs can repeat a name
		}
		switch c.State {
		case github.CheckFailed:
			seen[c.Name] = true
			d.FailedChecks = append(d.FailedChecks, c.Name)
		case github.CheckStale:
			seen[c.Name] = true
			stale = append(stale, c.Name)
		}
	}

	skip := func(format string, args ...any) Decision {
		d.Action, d.Reason = ActionSkip, fmt.Sprintf(format, args...)
		return d
	}
	// wait leaves the PR for a later run; inFlight says whether it holds
	// the rebase queue meanwhile.
	wait := func(inFlight bool, format string, args ...any) Decision {
		d.Action, d.Reason, d.inFlight = ActionWait, fmt.Sprintf(format, args...), inFlight
		return d
	}
	upToDate := pr.MergeState != "BEHIND"

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
	d.assessed = true
	if pr.ChecksOID != pr.HeadOID {
		d.assessed = false
		withinPolicy := d.Grade != config.UpdateUnknown && d.Grade <= p.MaxUpdateType
		return wait(withinPolicy && upToDate, "checks not yet reported for head commit %.7s", pr.HeadOID)
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
		return wait(upToDate, "%d check(s) still running", pending)
	case len(stale) > 0:
		return skip("stale checks: %s (rerun them or comment @dependabot recreate)", strings.Join(stale, ", "))
	case len(pr.Checks) == 0 && p.RequireChecks:
		if repo.FetchedAt.Sub(pushedAt(pr)) < checksGrace {
			return wait(upToDate, "no checks reported yet for a new commit")
		}
		return skip("no checks ran (set require_checks: false to opt in)")
	}

	switch pr.Mergeable {
	case "CONFLICTING":
		return skip("merge conflicts (Dependabot normally rebases; try @dependabot recreate)")
	case "UNKNOWN", "":
		return wait(upToDate, "GitHub is still computing mergeability")
	}

	var safe Decision
	switch pr.MergeState {
	case "CLEAN", "HAS_HOOKS":
		safe = d
		safe.Action, safe.Reason = ActionMerge, "checks green, within policy"
	case "BEHIND":
		if at, ok := rebaseRequested(pr, viewer); ok {
			if age := repo.FetchedAt.Sub(at); age >= rebaseTimeout {
				d.stalledRebase = true
				return skip("rebase requested %s ago and not done; left for a human", days(age))
			}
			return wait(true, "rebase already requested")
		}
		safe = d
		safe.Action, safe.Reason = ActionRebase, "behind base branch"
	case "BLOCKED":
		if pr.ReviewDecision == "REVIEW_REQUIRED" || pr.ReviewDecision == "CHANGES_REQUESTED" {
			d.needsReview = true
			return skip("branch protection requires an approving review")
		}
		return skip("blocked by branch protection")
	case "DIRTY":
		return skip("merge conflicts")
	case "UNKNOWN", "":
		return wait(true, "GitHub is still computing merge state")
	default:
		return skip("merge state %s", pr.MergeState)
	}

	if !repo.CanWrite() {
		safe.Action, safe.Reason = ActionSkip, fmt.Sprintf("would %s, but you only have %s access", safe.Action, strings.ToLower(repo.Permission))
	}
	return safe
}

// pushedAt is when the PR's latest commit was made, or the zero time when
// it has none.
func pushedAt(pr *github.PullRequest) time.Time {
	if len(pr.Commits) == 0 {
		return time.Time{}
	}
	return pr.Commits[len(pr.Commits)-1].CommittedDate
}

// rebaseRequested returns when viewer first asked for a rebase since the
// PR's latest commit, if it did.
func rebaseRequested(pr *github.PullRequest, viewer string) (time.Time, bool) {
	if len(pr.Commits) == 0 {
		return time.Time{}, false
	}
	last := pushedAt(pr)
	for _, c := range pr.Comments {
		if c.Author == viewer && c.CreatedAt.After(last) && strings.Contains(c.Body, RebaseCommand) {
			return c.CreatedAt, true
		}
	}
	return time.Time{}, false
}

// days describes an age of a day or more in whole days.
func days(age time.Duration) string {
	if n := int(age / (24 * time.Hour)); n > 1 {
		return fmt.Sprintf("%d days", n)
	}
	return "a day"
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
