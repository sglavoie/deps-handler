// Package policy decides, without side effects, what deps-merger should do
// with each Dependabot pull request and what the repository needs fixing.
package policy

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/dependabot"
	"github.com/sglavoie/deps-merger/pkg/github"
)

// Action is what deps-merger does with a pull request.
type Action string

const (
	ActionMerge  Action = "merge"  // safe: merge now
	ActionRebase Action = "rebase" // safe but behind: ask Dependabot to rebase
	ActionWait   Action = "wait"   // transient state: try again next run
	ActionSkip   Action = "skip"   // not safe: leave untouched
	// ActionReview: safe but for its size, which the agent must judge.
	ActionReview Action = "review"
)

// RebaseCommand is the comment that asks Dependabot to rebase a PR.
const RebaseCommand = "@dependabot rebase"

// AwaitingReview ends the reason of a PR waiting for the agent's review.
const AwaitingReview = "; awaiting agent review"

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
	FailedChecks []string                `json:"failed_checks"`
	// Verdict is the agent's review, for a PR the policy alone would not
	// merge because of its size.
	Verdict *Verdict `json:"verdict,omitempty"`
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
	// behind is true for a PR that would be rebased, but that the policy
	// leaves to a human: dependabot_commands is off.
	behind bool
	// heldForSize is true for a PR skipped for its size alone: it would
	// merge, rebase or wait (for its checks, say) if the policy allowed
	// updates that size.
	heldForSize bool
}

const (
	// checksGrace is how long a new commit may go without any check before
	// it counts as having no CI: GitHub registers checks only once a
	// workflow starts.
	checksGrace = config.ChecksGrace
	// rebaseTimeout is how long a rebase request may go unanswered before
	// the PR stops holding the rebase queue.
	rebaseTimeout = 24 * time.Hour
	// conflictGrace is how long after the base branch moves a PR it put in
	// conflict waits for Dependabot, which rebases such PRs on its own,
	// before it is left for a human.
	conflictGrace = 30 * time.Minute
)

// Verdict is what the AI agent concluded about one update.
type Verdict struct {
	Approved bool     `json:"approved"`
	Summary  string   `json:"summary"`
	Concerns []string `json:"concerns"`
	// Error says why the review could not be done; Approved is then false.
	Error string `json:"error,omitempty"`
}

// MarshalJSON writes no concerns as an empty list rather than null, which
// is what a failed review or an older cached verdict would otherwise give.
func (v Verdict) MarshalJSON() ([]byte, error) {
	type plain Verdict
	if v.Concerns == nil {
		v.Concerns = []string{}
	}
	return json.Marshal(plain(v))
}

// Judge returns the agent's verdict on a PR whose size the policy does not
// allow, or nil when the agent has not reviewed it. A nil Judge means agent
// reviews are off, and such PRs are skipped.
type Judge func(Decision) *Verdict

// approveAll approves every update, to tell whether size alone holds a PR.
func approveAll(Decision) *Verdict { return &Verdict{Approved: true} }

// Evaluate decides what to do with every Dependabot PR of repo. viewer is
// the authenticated login, used to avoid repeating our own rebase requests.
// judge supplies agent verdicts and may be nil.
func Evaluate(repo *github.Repo, p config.Policy, viewer string, judge Judge) []Decision {
	return EvaluateAfter(repo, p, viewer, judge, nil)
}

// EvaluateAfter is Evaluate later in a run that already failed to merge,
// or to ask to rebase, the PRs in refused: they still read as merges or
// rebases, but will not move before the next run, so they no longer hold
// the rebase queue.
func EvaluateAfter(repo *github.Repo, p config.Policy, viewer string, judge Judge, refused map[int]bool) []Decision {
	out := make([]Decision, 0, len(repo.PullRequests))
	for i := range repo.PullRequests {
		pr := &repo.PullRequests[i]
		if !dependabot.IsLogin(pr.Author) {
			continue // never touch PRs Dependabot did not open
		}
		d := evaluate(repo, pr, p, viewer, judge)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	queueRebases(out, refused)
	return out
}

// queueRebases keeps at most one PR moving towards the base branch at a
// time. When the base must be up to date, each merge puts every other PR
// behind again, so rebasing them all at once reruns CI on each of them for
// every merge. The rest wait behind the next merge or rebase; a merge or
// rebase in refused is not next.
func queueRebases(ds []Decision, refused map[int]bool) {
	ahead := 0
	for _, d := range ds {
		if d.Action == ActionMerge && !refused[d.Number] || d.inFlight {
			ahead = d.Number
			break
		}
	}
	for i := range ds {
		if ds[i].Action != ActionRebase || refused[ds[i].Number] {
			continue
		}
		if ahead == 0 {
			ahead = ds[i].Number
			continue
		}
		ds[i].Action, ds[i].Reason = ActionWait, fmt.Sprintf("queued behind #%d", ahead)
	}
}

// LatestChecks drops the check runs a newer suite of the same app,
// workflow and event reran under the same name, as GitHub does when it
// shows a pull request's checks: a run cancelled in favour of a later one
// says nothing about the update. Runs sharing a suite, like matrix jobs, all
// count, and so does every status context: GitHub reports only the latest
// of each. Same-named checks from different workflows or apps are different
// checks, and a failing "test" in one is never hidden by a passing "test" in
// another. So are the runs of one workflow for different events: one that
// listens to both push and pull_request runs twice on the same commit, and
// the newer run passing says nothing about the other failing. Nor does a
// newer run that was skipped or neutral, as one a label added after the PR
// opened can trigger: it judged nothing, so it hides nothing, and is kept
// beside the run it would otherwise have replaced. A check missing its
// name, suite, app, workflow or event is kept too: missing identities cannot
// establish that two runs came from the same source.
func LatestChecks(checks []github.Check) []github.Check {
	type origin struct {
		app, workflow int64
		event, name   string
	}
	identified := func(c github.Check) bool {
		return !c.Status && c.Name != "" && c.Suite > 0 && c.App > 0 && c.Workflow > 0 && c.Event != ""
	}
	newest := map[origin]int64{}
	for _, c := range checks {
		if identified(c) && c.State != github.CheckSkipped {
			o := origin{c.App, c.Workflow, c.Event, c.Name}
			newest[o] = max(newest[o], c.Suite)
		}
	}
	out := make([]github.Check, 0, len(checks))
	for _, c := range checks {
		if !identified(c) || c.State == github.CheckSkipped || c.Suite == newest[origin{c.App, c.Workflow, c.Event, c.Name}] {
			out = append(out, c)
		}
	}
	return out
}

// anyRan reports whether any of checks actually ran. A skipped or neutral
// check blocks nothing, but a commit whose checks were all skipped was
// tested by nothing either.
func anyRan(checks []github.Check) bool {
	for _, c := range checks {
		if c.State != github.CheckSkipped {
			return true
		}
	}
	return false
}

// inConflict reports whether GitHub says pr conflicts with its base. It
// then runs no pull_request workflow on it, so no checks is the conflict's
// doing, not a repository without CI.
func inConflict(pr *github.PullRequest) bool {
	return pr.Mergeable == "CONFLICTING" || pr.MergeState == "DIRTY"
}

func evaluate(repo *github.Repo, pr *github.PullRequest, p config.Policy, viewer string, judge Judge) Decision {
	d := Decision{PR: pr, Number: pr.Number, Title: pr.Title, URL: pr.URL, Dependencies: []dependabot.Dependency{}}
	// A commit whose size cannot be read makes the whole PR's unknown, not
	// just the PR's other commits' size.
	unread := false
	for _, c := range pr.Commits {
		deps := dependabot.ParseCommitMessage(c.Message)
		unread = unread || len(deps) == 0
		d.Dependencies = append(d.Dependencies, deps...)
	}
	d.Grade = dependabot.Grade(d.Dependencies)
	if unread {
		d.Grade = config.UpdateUnknown
	}
	checks := LatestChecks(pr.Checks)
	// Matrix jobs and other workflows can repeat a name; one that failed
	// anywhere is listed as failing, not stale, as rerunning won't fix it.
	failed, staled := map[string]bool{}, map[string]bool{}
	for _, c := range checks {
		if c.State == github.CheckFailed && !failed[c.Name] {
			failed[c.Name] = true
			d.FailedChecks = append(d.FailedChecks, c.Name)
		}
	}
	var stale []string
	for _, c := range checks {
		if c.State == github.CheckStale && !failed[c.Name] && !staled[c.Name] {
			staled[c.Name] = true
			stale = append(stale, c.Name)
		}
	}

	skip := func(format string, args ...any) Decision {
		d.Action, d.Reason = ActionSkip, fmt.Sprintf(format, args...)
		return d
	}
	// oversized says why the update is beyond what the policy merges on
	// its own; the agent may still approve it.
	oversized := ""
	switch {
	case d.Grade == config.UpdateUnknown:
		oversized = "cannot determine update type from Dependabot metadata"
	case d.Grade > p.MaxUpdateType:
		oversized = fmt.Sprintf("%s update exceeds limit (%s)", d.Grade, p.MaxUpdateType)
	}
	// wait leaves the PR for a later run; inFlight says whether it holds
	// the rebase queue meanwhile, which a PR not yet approved never does.
	wait := func(inFlight bool, format string, args ...any) Decision {
		d.Action, d.Reason = ActionWait, fmt.Sprintf(format, args...)
		d.inFlight = inFlight && (oversized == "" || d.Verdict != nil && d.Verdict.Approved)
		return d
	}
	// A PR in conflict will not merge next either, whatever its checks do.
	upToDate := pr.MergeState != "BEHIND" && pr.MergeState != "DIRTY" && pr.Mergeable != "CONFLICTING"
	// conflicts skips a PR that cannot merge cleanly, unless the base
	// branch moved since its latest commit, as when a sibling PR touching
	// the same lockfile just merged: Dependabot rebases it shortly.
	conflicts := func(hint string) Decision {
		if moved := pr.BaseMovedAt; moved.After(pushedAt(pr)) {
			if age := repo.FetchedAt.Sub(moved); age < conflictGrace {
				return wait(false, "merge conflicts since the base branch moved %s ago; waiting for Dependabot to rebase", config.ShortDuration(age.Truncate(time.Second)))
			}
		}
		return skip("merge conflicts%s", hint)
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
	if len(pr.Commits) == 0 {
		return skip("no commits to verify; left for a human")
	}
	for _, c := range pr.Commits {
		if len(c.Authors) == 0 {
			return skip("commit %.7s has no author; left for a human", c.OID)
		}
		// Dependabot's own commits always link their author to its
		// account; a name or email alone is anyone's to type. Only
		// Co-authored-by trailers, which link no account, are judged by
		// them.
		if a := c.Authors[0]; !dependabot.IsLogin(a.Login) {
			if a.Login == "" && a.Name == "" {
				return skip("commit %.7s author is not linked to Dependabot's account; left for a human", c.OID)
			}
			if a.Login == "" {
				return skip("commit %.7s author %q is not linked to Dependabot's account; left for a human", c.OID, a.Name)
			}
			return skip("modified by %s (commit %.7s); left for a human", a.Login, c.OID)
		}
		for _, a := range c.Authors[1:] {
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
		if !c.CommittedByGitHub {
			return skip("commit %.7s was not committed by GitHub; left for a human", c.OID)
		}
	}
	// The commit list and head/checks are separate GraphQL fields. If a
	// push leaves them out of sync, pinning a merge to HeadOID is not
	// enough: that head's authorship and metadata must be verified too.
	if pr.HeadOID == "" || !slices.ContainsFunc(pr.Commits, func(c github.Commit) bool { return c.OID == pr.HeadOID }) {
		return wait(false, "commits not yet reported for head commit %.7s", pr.HeadOID)
	}
	d.assessed = true
	if oversized != "" && judge != nil && p.AgentReview {
		d.Verdict = judge(d)
	}
	if pr.ChecksOID != pr.HeadOID {
		d.assessed = false
		return wait(upToDate, "checks not yet reported for head commit %.7s", pr.HeadOID)
	}
	if oversized != "" {
		switch v := d.Verdict; {
		case judge == nil || !p.AgentReview:
			reviewed := p
			reviewed.AgentReview = true
			// A PR left behind for a human would still rebase.
			reviewed.DependabotCommands = true
			switch evaluate(repo, pr, reviewed, viewer, approveAll).Action {
			case ActionMerge, ActionRebase, ActionWait:
				d.heldForSize = true
			}
			return skip("%s", oversized)
		case v != nil && v.Error != "":
			return skip("%s; agent review failed: %s", oversized, v.Error)
		case v != nil && !v.Approved:
			return skip("%s; rejected by agent", oversized)
		}
	}

	if pr.CheckCount > len(pr.Checks) {
		return skip("too many checks to verify (%d)", pr.CheckCount)
	}
	// Validate the whole response before relying on supersession: a newer
	// suite cannot establish what an unrecognized older state means.
	for _, c := range pr.Checks {
		switch c.State {
		case github.CheckPending, github.CheckPassed, github.CheckFailed, github.CheckSkipped, github.CheckStale:
		default:
			return skip("unknown check state %q for %s; left for a human", c.State, c.Name)
		}
	}
	pending := 0
	for _, c := range checks {
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
	case !anyRan(checks) && inConflict(pr):
		return conflicts(conflictHint(pr))
	case !anyRan(checks) && p.RequireChecks:
		none := "no checks reported yet"
		if len(checks) > 0 {
			none = "only skipped checks reported yet"
		}
		age := repo.FetchedAt.Sub(pushedAt(pr))
		// The config already waits at least checksGrace; a policy built
		// another way must not merge before CI has had time to start.
		after := p.MergeWithoutChecksAfter
		if after > 0 {
			after = max(after, checksGrace)
		}
		switch {
		case after > 0 && age < after:
			// Rounded up: a countdown never reads 0s while it still waits.
			left := (after - age + time.Minute - 1).Truncate(time.Minute)
			return wait(upToDate, "%s; merging without them in %s", none, config.ShortDuration(left))
		case after > 0:
			// No CI started in time: merge as if checks were optional.
		case age < checksGrace:
			return wait(upToDate, "%s for a new commit", none)
		default:
			return skip("no checks ran (set require_checks: false or merge_without_checks_after to opt in)")
		}
	}

	switch pr.Mergeable {
	case "MERGEABLE":
	case "CONFLICTING":
		return conflicts(conflictHint(pr))
	case "UNKNOWN", "":
		return wait(upToDate, "GitHub is still computing mergeability")
	default:
		return skip("unknown mergeability %q; left for a human", pr.Mergeable)
	}

	var safe Decision
	switch pr.MergeState {
	case "CLEAN", "HAS_HOOKS":
		safe = d
		safe.Action, safe.Reason = ActionMerge, "checks green, within policy"
		if !anyRan(checks) {
			safe.Reason = "no checks ran, within policy"
		}
	case "BEHIND":
		// Without Dependabot commands no request is made, so none is
		// waited on: the PR is left to a human below.
		if at, seen, ok := rebaseRequested(pr, viewer); ok && p.DependabotCommands {
			requested := "rebase requested"
			if !seen {
				requested = "rebase may have been requested (older comments not read)"
			}
			if age := repo.FetchedAt.Sub(at); age >= rebaseTimeout {
				d.stalledRebase = true
				return skip("%s %s ago and not done; left for a human", requested, days(age))
			}
			if !seen {
				return wait(true, "%s", requested)
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
		return conflicts(conflictHint(pr))
	case "UNKNOWN", "":
		return wait(true, "GitHub is still computing merge state")
	default:
		return skip("merge state %s", pr.MergeState)
	}

	if oversized != "" {
		if safe.Verdict == nil {
			safe.Action, safe.Reason = ActionReview, oversized+AwaitingReview
		} else if safe.Action == ActionMerge {
			// "checks green" or "no checks ran", as within policy.
			safe.Reason = strings.TrimSuffix(safe.Reason, "within policy") + fmt.Sprintf("%s update approved by agent", safe.Grade)
		} else {
			safe.Reason += fmt.Sprintf(" (%s update approved by agent)", safe.Grade)
		}
	}
	if !repo.CanWrite() {
		safe.Action, safe.Reason = ActionSkip, fmt.Sprintf("would %s, but %s", safe.Action, lacksWrite(repo))
	} else if safe.Action == ActionRebase && !p.DependabotCommands {
		safe.Action, safe.Reason = ActionSkip, "behind base branch; rebase left to a human (dependabot_commands: false)"
		if oversized != "" {
			// Whoever rebases it should know the approval carries over.
			safe.Reason += fmt.Sprintf("; %s update approved by agent", safe.Grade)
		}
		safe.behind = true
	}
	return safe
}

// lacksWrite says what access the viewer has instead of write access.
// GitHub reports none for a GitHub App installation, unless the caller
// fills it in with github.WithPermission.
func lacksWrite(repo *github.Repo) string {
	if repo.Permission == "" {
		return "GitHub reports no access for you"
	}
	return fmt.Sprintf("you only have %s access", strings.ToLower(repo.Permission))
}

// conflictHint is what the skip of a PR in conflict suggests, from how
// GitHub reports the conflict.
func conflictHint(pr *github.PullRequest) string {
	if pr.Mergeable == "CONFLICTING" {
		return " (Dependabot normally rebases; try @dependabot recreate)"
	}
	return ""
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
// PR's latest commit, if it did; seen is false when the request may only
// hide among comments that were not fetched. Only the latest comments are:
// when older ones were cut off and even the oldest fetched one is newer
// than the commit, a request counts as made no later than that comment, so
// a busy PR is not asked blindly again and still stalls in time.
func rebaseRequested(pr *github.PullRequest, viewer string) (at time.Time, seen, ok bool) {
	if len(pr.Commits) == 0 {
		return time.Time{}, false, false
	}
	last := pushedAt(pr)
	for _, c := range pr.Comments {
		if c.Author == viewer && c.CreatedAt.After(last) && strings.Contains(c.Body, RebaseCommand) {
			return c.CreatedAt, true, true
		}
	}
	if pr.CommentCount > len(pr.Comments) && len(pr.Comments) > 0 && pr.Comments[0].CreatedAt.After(last) {
		return pr.Comments[0].CreatedAt, false, true
	}
	return time.Time{}, false, false
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
