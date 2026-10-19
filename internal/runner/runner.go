// Package runner drives one pass over the configured repositories: fetch,
// evaluate, and (with Apply) merge safe PRs one at a time.
package runner

import (
	"context"
	"errors"
	"fmt"
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
	// to be told apart by whether the PR is still open.
	unsure := map[int]Event{}
	polls := 0
	rv := &repoReviews{verdicts: map[string]*policy.Verdict{}, moved: map[int]bool{}, res: &res}
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
			return res
		}
		for n, ev := range unsure {
			delete(unsure, n)
			if !slices.ContainsFunc(repo.PullRequests, func(pr github.PullRequest) bool { return pr.Number == n }) {
				attempted[n] = true
				res.Events = append(res.Events, ev)
			}
		}
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
			return res
		}

		for _, d := range decisions {
			if d.Action != policy.ActionRebase || rebased[d.Number] {
				continue
			}
			rebased[d.Number] = true
			ev := Event{Number: d.Number, Title: d.Title, URL: d.URL, Grade: d.Grade, Kind: EventRebaseRequested, Detail: r.approval(d)}
			if err := r.comment(ctx, rc, d.Number, policy.RebaseCommand); err != nil {
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
			return res
		}

		attempted[next.Number] = true
		ev := Event{Number: next.Number, Title: next.Title, URL: next.URL, Grade: next.Grade, Kind: EventMerged}
		err = methodErr
		if err == nil {
			err = r.merge(ctx, rc, next.Number, method, next.PR.HeadOID)
		}
		uncertain := err != nil && github.Transient(err)
		if err != nil && (github.Retryable(err) || uncertain) && !retried[next.Number] {
			// GitHub may not have caught up with the previous merge yet:
			// wait, refetch, and try this PR once more. A merge that timed
			// out may have gone through: if the PR is gone, it did.
			retried[next.Number] = true
			delete(attempted, next.Number)
			if uncertain {
				ev.Detail = joinDetail(method, r.approval(*next), "after "+err.Error())
				unsure[next.Number] = ev
			}
			if err := r.Sleep(ctx, r.PollInterval); err != nil {
				res.Error = errText(ctx, err)
				return res
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
	// reviewed at most once per run. A nil verdict leaves the PR for the
	// next run.
	verdicts map[string]*policy.Verdict
	// moved holds the PRs whose head moved under a review once, by
	// number: the key of an update without versions moves with its head.
	moved map[int]bool
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
		req := review.NewRequest(rc, *d)
		key := req.Key()
		if v, done := rv.verdicts[key]; done {
			if v == nil && !strings.HasSuffix(d.Reason, headMoved) {
				d.Reason += headMoved
			}
			continue // otherwise an approval the PR has not caught up with yet
		}
		if r.MaxReviews > 0 && r.reviewsStarted.Add(1) > int64(r.MaxReviews) {
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
			// head that keeps moving cannot hold the run.
			r.reviewsStarted.Add(-1)
			if rv.moved[d.Number] {
				rv.verdicts[key] = nil
			}
			rv.moved[d.Number] = true
			return true
		}
		if err != nil {
			v = policy.Verdict{Error: errText(ctx, err)}
		} else if r.Verdicts != nil {
			if err := r.Verdicts.Put(key, req.HeadOID, r.AgentName, v); err != nil {
				rv.res.Warnings = append(rv.res.Warnings, fmt.Sprintf("#%d: verdict not cached: %v", d.Number, err))
			}
		}
		rv.verdicts[key] = &v
		return true
	}
	return false
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
