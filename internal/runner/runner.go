// Package runner drives one pass over the configured repositories: fetch,
// evaluate, and (with Apply) merge safe PRs one at a time.
package runner

import (
	"context"
	"time"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/policy"
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
	Error       string            `json:"error,omitempty"`
}

// Failed reports whether the repository could not be processed or a write
// to it failed.
func (r RepoResult) Failed() bool {
	if r.Error != "" {
		return true
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
}

func New(c github.Client, viewer string, apply bool) *Runner {
	return &Runner{Client: c, Viewer: viewer, Apply: apply, PollAttempts: 5, PollInterval: 3 * time.Second, Sleep: sleep}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run processes repos in order; a failure in one repo does not stop others.
func (r *Runner) Run(ctx context.Context, repos []config.Repo, progress func(RepoResult)) []RepoResult {
	out := make([]RepoResult, 0, len(repos))
	for _, rc := range repos {
		res := r.RunRepo(ctx, rc)
		if progress != nil {
			progress(res)
		}
		out = append(out, res)
		if ctx.Err() != nil {
			break
		}
	}
	return out
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
	polls := 0

	for {
		repo, err := r.Client.FetchRepo(ctx, rc.Owner, rc.Name)
		if err != nil {
			res.Error = errText(ctx, err)
			return res
		}
		res.Permission = repo.Permission
		method, methodErr := policy.MergeMethod(repo, rc.Policy.MergeMethod)
		res.MergeMethod = method
		decisions := policy.Evaluate(repo, rc.Policy, r.Viewer)
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
			ev := Event{Number: d.Number, Title: d.Title, Grade: d.Grade, Kind: EventRebaseRequested}
			if err := r.Client.Comment(ctx, rc.Owner, rc.Name, d.Number, policy.RebaseCommand); err != nil {
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
		ev := Event{Number: next.Number, Title: next.Title, Grade: next.Grade, Kind: EventMerged}
		err = methodErr
		if err == nil {
			err = r.Client.Merge(ctx, rc.Owner, rc.Name, next.Number, method, next.PR.HeadOID)
		}
		if err != nil && github.Retryable(err) && !retried[next.Number] {
			// GitHub may not have caught up with the previous merge yet:
			// wait, refetch, and try this PR once more.
			retried[next.Number] = true
			delete(attempted, next.Number)
			if err := r.Sleep(ctx, r.PollInterval); err != nil {
				res.Error = errText(ctx, err)
				return res
			}
			continue
		}
		if err != nil {
			ev.Kind, ev.Detail = EventMergeFailed, errText(ctx, err)
		} else {
			ev.Detail = method
			polls = 0
		}
		res.Events = append(res.Events, ev)
	}
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
