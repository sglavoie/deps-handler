// Package runner drives one pass over the configured repositories: fetch,
// evaluate, and (with Apply) merge safe PRs one at a time.
package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/policy"
	"github.com/sglavoie/deps-merger/internal/review"
)

// EventKind is a write deps-merger performed (or tried to).
type EventKind string

const (
	EventMerged          EventKind = "merged"
	EventMergeFailed     EventKind = "merge-failed"
	EventRebaseRequested EventKind = "rebase-requested"
	EventRebaseFailed    EventKind = "rebase-failed"
)

type Event struct {
	Number int               `json:"number"`
	Title  string            `json:"title"`
	URL    string            `json:"url"`
	Grade  config.UpdateType `json:"grade"`
	Kind   EventKind         `json:"kind"`
	Detail string            `json:"detail,omitempty"`
}

// RepoResult is everything a run learned and did in one repository.
type RepoResult struct {
	Repo       string        `json:"repo"`
	Policy     config.Policy `json:"policy"`
	Permission string        `json:"permission,omitempty"`
	// MergeMethod is the method merges use: the policy's, or the first one
	// the repository allows when it does not allow that.
	MergeMethod string            `json:"merge_method,omitempty"`
	Decisions   []policy.Decision `json:"decisions"`
	Findings    []policy.Finding  `json:"findings"`
	Events      []Event           `json:"events"`
	// Warnings are problems that did not stop the run, such as a verdict
	// that could not be cached.
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// Failed reports whether the repository could not be processed, a write
// to it failed, or an agent review could not be done.
func (r RepoResult) Failed() bool {
	if r.Error != "" {
		return true
	}
	for _, d := range r.Decisions {
		if d.Verdict != nil && d.Verdict.Error != "" {
			return true
		}
	}
	for _, ev := range r.Events {
		if ev.Kind == EventMergeFailed || ev.Kind == EventRebaseFailed {
			return true
		}
	}
	return false
}

type Runner struct {
	Client github.Client
	Viewer string
	Apply  bool

	// PollAttempts and PollInterval bound how long to wait for GitHub to
	// recompute mergeability after a merge.
	PollAttempts int
	PollInterval time.Duration
	Sleep        func(context.Context, time.Duration) error

	// Parallel is how many repositories are processed at once.
	Parallel int
	// writes serialises merges and comments across repositories: GitHub
	// asks clients not to make concurrent writes for one user.
	writes sync.Mutex

	// Reviewer, when set, has an agent judge PRs the policy alone would
	// not merge because of their size.
	Reviewer review.Reviewer
	// AgentName is recorded with each cached verdict.
	AgentName string
	// Verdicts keeps verdicts between runs; nil keeps them for this run.
	Verdicts *review.Cache
	// MaxReviews caps agent reviews across the whole run; 0 is no cap.
	MaxReviews int
	// ReviewParallel is how many agent reviews run at once.
	ReviewParallel int
	reviewsOnce    sync.Once
	reviewSlots    chan struct{}
	reviewsStarted atomic.Int64

	// warnings are the run's own, given to the first repository's result.
	warnings []string
}

// Warn adds a warning about the run as a whole, such as a verdict cache
// found unreadable as the run starts, to be reported with the first
// repository's result: under its warnings in JSON, below it in the text
// report. Call it before Run; it is lost if no repository runs.
func (r *Runner) Warn(msg string) {
	r.warnings = append(r.warnings, msg)
}

func New(c github.Client, viewer string, apply bool) *Runner {
	return &Runner{Client: c, Viewer: viewer, Apply: apply, PollAttempts: 5, PollInterval: 3 * time.Second, Sleep: github.Sleep, Parallel: 4, ReviewParallel: 2}
}

// Run processes up to Parallel repos at a time and returns their results in
// the order given, passing each to progress in that order as soon as it and
// those before it are done. A failure in one repo does not stop others; an
// interruption stops repos from starting, and those are left out.
func (r *Runner) Run(ctx context.Context, repos []config.Repo, progress func(RepoResult)) []RepoResult {
	results := make([]RepoResult, len(repos))
	// started[i] says whether repo i ran, once results[i] is final.
	started := make([]chan bool, len(repos))
	for i := range started {
		started[i] = make(chan bool, 1)
	}
	go func() {
		slots := make(chan struct{}, max(1, r.Parallel))
		for i, rc := range repos {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				for _, s := range started[i:] {
					s <- false
				}
				return
			}
			go func() {
				results[i] = r.RunRepo(ctx, rc)
				<-slots
				started[i] <- true
			}()
		}
	}()

	out := make([]RepoResult, 0, len(repos))
	for i := range repos {
		if !<-started[i] {
			break
		}
		if i == 0 && len(r.warnings) > 0 {
			results[0].Warnings = append(slices.Clone(r.warnings), results[0].Warnings...)
		}
		if progress != nil {
			progress(results[i])
		}
		out = append(out, results[i])
	}
	return out
}

func (r *Runner) merge(ctx context.Context, rc config.Repo, number int, method, headOID string) error {
	return r.write(ctx, func() error { return r.Client.Merge(ctx, rc.Owner, rc.Name, number, method, headOID) })
}

func (r *Runner) comment(ctx context.Context, rc config.Repo, number int, body string) error {
	return r.write(ctx, func() error { return r.Client.Comment(ctx, rc.Owner, rc.Name, number, body) })
}

// write makes one write at a time across repositories, once more after
// the pause GitHub asks for when it refuses it for a rate limit. The pause
// holds back the other repositories' writes too: the limit is the user's.
func (r *Runner) write(ctx context.Context, f func() error) error {
	r.writes.Lock()
	defer r.writes.Unlock()
	err := f()
	if d, ok := github.RateLimited(err); ok && d <= github.MaxRateLimitWait && ctx.Err() == nil {
		if err := r.Sleep(ctx, d); err != nil {
			return err
		}
		err = f()
	}
	return err
}

func (r *Runner) RunRepo(ctx context.Context, rc config.Repo) RepoResult {
	res := RepoResult{
		Repo:      rc.FullName(),
		Policy:    rc.Policy,
		Decisions: []policy.Decision{},
		Findings:  []policy.Finding{},
		Events:    []Event{},
	}
	attempted := map[int]bool{}
	retried := map[int]bool{}
	rebased := map[int]bool{}
	// unsure holds merges GitHub failed in a way that may hide a success,
	// until the pass tells whether they went through. doubted keeps, for
	// the rest of the pass, why a PR's merge may have gone through.
	unsure := map[int]pendingMerge{}
	doubted := map[int]string{}
	// resolve reports unsure merge n as it turned out.
	resolve := func(n int, ev Event) {
		delete(unsure, n)
		attempted[n] = true
		res.Events = append(res.Events, ev)
	}
	// settle reports the merges in unsure that repo, the repository read
	// again, tells apart. One whose PR is still open was not merged: it
	// failed if it was already tried twice, and is otherwise kept for one
	// more try. One whose PR is no longer open may have merged or been
	// closed meanwhile, which GitHub is asked; if it cannot say, finish
	// asks again.
	settle := func(repo *github.Repo) {
		for _, n := range slices.Sorted(maps.Keys(unsure)) {
			p := unsure[n]
			if !slices.ContainsFunc(repo.PullRequests, func(pr github.PullRequest) bool { return pr.Number == n }) {
				state, err := r.prState(ctx, rc, n)
				if err != nil {
					continue
				}
				if state != github.PROpen {
					resolve(n, p.outcome(state))
					continue
				}
				// Open after all: the search had not caught up.
			}
			if p.failure != "" {
				resolve(n, p.failed(p.failure))
			}
		}
	}
	// finish reports the merges still in unsure as the pass ends, such as
	// one whose PR became unmergeable for a while, so that none goes
	// unreported, and returns the result. Each is reported as GitHub says
	// it turned out, and failed when it cannot say.
	finish := func() RepoResult {
		for _, n := range slices.Sorted(maps.Keys(unsure)) {
			p := unsure[n]
			switch state, err := r.prState(ctx, rc, n); {
			case err != nil:
				resolve(n, p.failed(fmt.Sprintf("may have merged after %s, but GitHub could not say: %s", p.cause, errText(ctx, err))))
			case state != github.PROpen:
				resolve(n, p.outcome(state))
			case p.failure != "":
				resolve(n, p.failed(p.failure))
			default:
				resolve(n, p.failed(fmt.Sprintf("not merged after %s, and not mergeable again in this run", p.cause)))
			}
		}
		return res
	}
	polls := 0
	rv := &repoReviews{verdicts: map[string]*policy.Verdict{}, moved: map[int]int{}, res: &res}
	var judge policy.Judge
	if r.Reviewer != nil {
		judge = func(d policy.Decision) *policy.Verdict {
			key := review.NewRequest(rc, d).Key()
			if v, ok := rv.verdicts[key]; ok {
				return v
			}
			if r.Verdicts != nil {
				return r.Verdicts.Get(key, d.PR.HeadOID)
			}
			return nil
		}
	}

	for {
		repo, err := r.fetch(ctx, rc)
		if err != nil {
			res.Error = errText(ctx, err)
			return finish()
		}
		settle(repo)
		res.Permission = repo.Permission
		method, methodErr := policy.MergeMethod(repo, rc.Policy.MergeMethod)
		res.MergeMethod = method
		decisions := policy.Evaluate(repo, rc.Policy, r.Viewer, judge)
		res.Decisions = decisions
		res.Findings = policy.Diagnose(repo, rc.Policy, decisions)
		// GitHub computes mergeability lazily: the first query after a push
		// or a merge often says UNKNOWN, and asking again resolves it.
		poll := func() (bool, error) {
			if !recomputing(decisions) || polls >= r.PollAttempts {
				return false, nil
			}
			polls++
			return true, r.Sleep(ctx, r.PollInterval)
		}
		if !r.Apply {
			if r.reviewNext(ctx, rc, decisions, rv) {
				continue
			}
			again, err := poll()
			if err != nil {
				res.Error = errText(ctx, err)
			}
			if again && err == nil {
				continue
			}
			return finish()
		}

		for _, d := range decisions {
			if d.Action != policy.ActionRebase || rebased[d.Number] {
				continue
			}
			rebased[d.Number] = true
			ev := Event{Number: d.Number, Title: d.Title, URL: d.URL, Grade: d.Grade, Kind: EventRebaseRequested, Detail: r.approval(d)}
			switch err := r.comment(ctx, rc, d.Number, policy.RebaseCommand); {
			case err == nil:
			case github.Transient(err) && ctx.Err() == nil:
				// A comment that timed out or met a 5xx may well have been
				// posted, and asking twice is harmless while failing the
				// run over it is not: report it requested, with a warning.
				// If it was not posted, the next run asks again.
				ev.Detail = joinDetail(ev.Detail, "may not have been posted: "+err.Error())
				res.Warnings = append(res.Warnings, fmt.Sprintf("#%d: the rebase request may not have been posted: %v", d.Number, err))
			default:
				ev.Kind, ev.Detail = EventRebaseFailed, errText(ctx, err)
			}
			res.Events = append(res.Events, ev)
		}

		var next *policy.Decision
		for i := range decisions {
			if decisions[i].Action == policy.ActionMerge && !attempted[decisions[i].Number] {
				next = &decisions[i]
				break
			}
		}
		if next == nil {
			// Reviews come after merges: they take minutes, and the PRs
			// that need none should not wait on them.
			if r.reviewNext(ctx, rc, decisions, rv) {
				continue
			}
			again, err := poll()
			if err != nil {
				res.Error = errText(ctx, err)
			}
			if again && err == nil {
				continue
			}
			return finish()
		}

		attempted[next.Number] = true
		// This try supersedes an earlier one that may have gone through:
		// it merges, or fails in a way that tells whether that one did.
		delete(unsure, next.Number)
		ev := Event{Number: next.Number, Title: next.Title, URL: next.URL, Grade: next.Grade, Kind: EventMerged}
		err = methodErr
		if err == nil {
			err = r.merge(ctx, rc, next.Number, method, next.PR.HeadOID)
		}
		uncertain := err != nil && github.Transient(err)
		if uncertain && doubted[next.Number] == "" {
			doubted[next.Number] = err.Error()
		}
		if err != nil && (github.Retryable(err) || uncertain) && !retried[next.Number] {
			// GitHub may not have caught up with the previous merge yet:
			// wait, refetch, and try this PR once more. A merge that timed
			// out may have gone through: if the PR is gone, it did.
			retried[next.Number] = true
			delete(attempted, next.Number)
			if uncertain {
				ev.Detail = joinDetail(method, r.approval(*next), "after "+err.Error())
				unsure[next.Number] = pendingMerge{merged: ev, cause: err.Error()}
			}
			if err := r.Sleep(ctx, r.PollInterval); err != nil {
				res.Error = errText(ctx, err)
				return finish()
			}
			continue
		}
		if cause := doubted[next.Number]; err != nil && cause != "" && ctx.Err() == nil {
			// One of the attempts may have merged the PR, and GitHub can
			// still show it open a moment later, so the retry's 405 or 409
			// may only mean it is already merged: read it again before
			// calling the merge failed.
			ev.Detail = joinDetail(method, r.approval(*next), "after "+cause)
			unsure[next.Number] = pendingMerge{merged: ev, cause: cause, failure: errText(ctx, err)}
			if err := r.Sleep(ctx, r.PollInterval); err != nil {
				res.Error = errText(ctx, err)
				return finish()
			}
			continue
		}
		if err != nil {
			ev.Kind, ev.Detail = EventMergeFailed, errText(ctx, err)
		} else {
			ev.Detail = joinDetail(method, r.approval(*next))
			polls = 0
		}
		res.Events = append(res.Events, ev)
	}
}

// pendingMerge is a merge GitHub failed in a way that may hide a success.
type pendingMerge struct {
	// merged is the event to report if the PR is no longer open.
	merged Event
	// cause is the failure that may hide a success.
	cause string
	// failure is reported if the PR is still open and was already tried
	// twice; when empty, it is tried once more instead.
	failure string
}

// outcome is the event for a merge whose PR GitHub says is now in state,
// other than open: merged, or closed by someone without being merged.
func (p pendingMerge) outcome(state github.PRState) Event {
	if state == github.PRMerged {
		return p.merged
	}
	return p.failed("closed without being merged, after " + p.cause)
}

// failed is the event for a merge that did not go through.
func (p pendingMerge) failed(detail string) Event {
	ev := p.merged
	ev.Kind, ev.Detail = EventMergeFailed, detail
	return ev
}

// prState asks GitHub whether PR number is open, merged or closed, once
// more when it fails in a way that may pass.
func (r *Runner) prState(ctx context.Context, rc config.Repo, number int) (github.PRState, error) {
	return github.Again(ctx, r.PollInterval, r.Sleep, func() (github.PRState, error) {
		return r.Client.PullRequestState(ctx, rc.Owner, rc.Name, number)
	})
}

// fetch reads the repository, once more when GitHub fails in a way that
// may pass.
func (r *Runner) fetch(ctx context.Context, rc config.Repo) (*github.Repo, error) {
	return github.Again(ctx, r.PollInterval, r.Sleep, func() (*github.Repo, error) {
		return r.Client.FetchRepo(ctx, rc.Owner, rc.Name)
	})
}

// approval notes that the agent approved d, for the event acting on it.
func (r *Runner) approval(d policy.Decision) string {
	if d.Verdict == nil || !d.Verdict.Approved {
		return ""
	}
	if r.AgentName == "" {
		return "approved by agent"
	}
	return "approved by " + r.AgentName
}

// joinDetail joins the non-empty parts of an event's detail.
func joinDetail(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), ", ")
}

// repoReviews is what one repository's pass knows about its reviews.
type repoReviews struct {
	// verdicts holds this run's reviews, failed ones included, so a PR is
	// reviewed at most once per run.
	verdicts map[string]*policy.Verdict
	// moved counts, by PR number, the reviews a PR's head moved under. It
	// is kept by number, not by key: the key of an update without versions
	// moves with its head, so each new head would look unreviewed.
	moved map[int]int
	res   *RepoResult
}

// headMoved is the note on a PR whose head kept moving under its review.
const headMoved = " (head moved during review; next run)"

// reviewNext has the agent review the first PR awaiting review and reports
// whether it did, after which the repository must be evaluated again. When
// the run's review cap is reached, it says so on the PRs left waiting.
func (r *Runner) reviewNext(ctx context.Context, rc config.Repo, decisions []policy.Decision, rv *repoReviews) bool {
	for i := range decisions {
		d := &decisions[i]
		if d.Action != policy.ActionReview {
			continue
		}
		if rv.moved[d.Number] > 1 {
			if !strings.HasSuffix(d.Reason, headMoved) {
				d.Reason += headMoved
			}
			continue
		}
		req := review.NewRequest(rc, *d)
		key := req.Key()
		if _, done := rv.verdicts[key]; done {
			continue // an approval the PR has not caught up with yet
		}
		if !r.startReview() {
			for j := i; j < len(decisions); j++ {
				if decisions[j].Action == policy.ActionReview {
					decisions[j].Reason += fmt.Sprintf(" (review limit of %d reached)", r.MaxReviews)
				}
			}
			return false
		}
		v, err := r.review(ctx, req)
		if errors.Is(err, review.ErrHeadMoved) && ctx.Err() == nil {
			// Dependabot pushed while the review started: not a failure.
			// Evaluate the new head, and review it once more at most, so a
			// head that keeps moving cannot hold the run or use up reviews:
			// after a second move the PR waits for the next run.
			r.endReview()
			rv.moved[d.Number]++
			return true
		}
		if err != nil {
			v = policy.Verdict{Concerns: []string{}, Error: errText(ctx, err)}
		} else if r.Verdicts != nil {
			if err := r.Verdicts.Put(key, req.HeadOID, r.AgentName, v); err != nil {
				rv.res.Warnings = append(rv.res.Warnings, fmt.Sprintf("#%d: verdict not cached: %v", d.Number, err))
			}
			if msg := r.Verdicts.TakeRecovered(); msg != "" {
				rv.res.Warnings = append(rv.res.Warnings, msg)
			}
		}
		rv.verdicts[key] = &v
		return true
	}
	return false
}

// startReview takes one of the run's MaxReviews reviews, unless they are
// all taken. A refused review takes none, so a review given back later
// (endReview) can still be started by whichever repository asks next.
func (r *Runner) startReview() bool {
	if r.MaxReviews <= 0 {
		return true
	}
	for {
		n := r.reviewsStarted.Load()
		if n >= int64(r.MaxReviews) {
			return false
		}
		if r.reviewsStarted.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// endReview gives back a review startReview took that did not count.
func (r *Runner) endReview() {
	if r.MaxReviews > 0 {
		r.reviewsStarted.Add(-1)
	}
}

func (r *Runner) review(ctx context.Context, req review.Request) (policy.Verdict, error) {
	r.reviewsOnce.Do(func() { r.reviewSlots = make(chan struct{}, max(1, r.ReviewParallel)) })
	select {
	case r.reviewSlots <- struct{}{}:
	case <-ctx.Done():
		return policy.Verdict{}, ctx.Err()
	}
	defer func() { <-r.reviewSlots }()
	return r.Reviewer.Review(ctx, req)
}

// errText describes err, or just "interrupted" when it comes from the run
// being cancelled, which would otherwise read like a network failure.
func errText(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "interrupted"
	}
	return err.Error()
}

// recomputing reports whether GitHub is still working out whether some PR
// can merge, which typically follows a merge into the base branch.
func recomputing(decisions []policy.Decision) bool {
	for _, d := range decisions {
		if d.Action == policy.ActionWait && (d.PR.Mergeable == "UNKNOWN" || d.PR.MergeState == "UNKNOWN") {
			return true
		}
	}
	return false
}
