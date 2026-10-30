// Package runner drives one pass over the configured repositories: fetch,
// evaluate, and (with Apply) merge safe PRs one at a time.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/review"
)

// EventKind is a write deps-handler performed (or tried to).
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
	Repo   string        `json:"repo"`
	Policy config.Policy `json:"policy"`
	// ConfigFiles are the policy files the repository's policy was read
	// from, besides the config: config.OrgFile in its owner's
	// config.OrgRepo, as owner/.github:deps-handler.yml, then its own
	// config.RepoFile.
	ConfigFiles []string `json:"config_files"`
	Permission  string   `json:"permission"`
	// MergeMethod is the method merges use: the policy's, or the first one
	// the repository allows when it does not allow that.
	MergeMethod string            `json:"merge_method"`
	Decisions   []policy.Decision `json:"decisions"`
	Findings    []policy.Finding  `json:"findings"`
	Events      []Event           `json:"events"`
	// Warnings are problems that did not stop the run, such as a verdict
	// that could not be cached.
	Warnings []string `json:"warnings"`
	Error    string   `json:"error,omitempty"`
}

// MarshalJSON writes every list, empty ones as [] rather than null, those
// of its decisions included, so a script can read them without checking
// for them first.
func (r RepoResult) MarshalJSON() ([]byte, error) {
	type plain RepoResult
	r.Decisions = slices.Clone(nonNil(r.Decisions))
	for i := range r.Decisions {
		r.Decisions[i].Dependencies = nonNil(r.Decisions[i].Dependencies)
		r.Decisions[i].FailedChecks = nonNil(r.Decisions[i].FailedChecks)
	}
	r.ConfigFiles = nonNil(r.ConfigFiles)
	r.Findings = nonNil(r.Findings)
	r.Events = nonNil(r.Events)
	r.Warnings = nonNil(r.Warnings)
	return json.Marshal(plain(r))
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
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
	// recompute mergeability after a merge. Sleep waits for a duration
	// unless the context ends first; nil uses a timer.
	PollAttempts int
	PollInterval time.Duration
	Sleep        func(context.Context, time.Duration) error

	// Parallel is how many repositories are processed at once.
	Parallel int
	// writes serialises merges and comments across repositories: GitHub
	// asks clients not to make concurrent writes for one user.
	writes sync.Mutex
	// writesBlocked is set once a rate limit refused a write and was not
	// waited out, or refused it again after it was; later writes in the
	// run are not sent.
	writesBlocked atomic.Bool

	// Reviewer, when set, has an agent judge PRs the policy alone would
	// not merge because of their size.
	Reviewer review.Reviewer
	// AgentName is recorded with each cached verdict.
	AgentName string
	// Verdicts keeps verdicts between runs; nil keeps them for this run.
	Verdicts *review.Cache
	// verdictsMu keeps storing a verdict and asking Verdicts whether that
	// found the cache file unreadable together, so the warning goes to the
	// repository whose verdict found it.
	verdictsMu sync.Mutex
	// MaxReviews caps agent reviews across the whole run; 0 is no cap.
	MaxReviews int
	// ReviewParallel is how many agent reviews run at once.
	ReviewParallel int
	reviewsOnce    sync.Once
	reviewSlots    chan struct{}
	reviewsStarted atomic.Int64

	// warnings are the run's own, given to the first repository's result.
	warnings []string

	// orgFiles reads each owner's config.OrgFile once per run.
	orgFilesMu sync.Mutex
	orgFiles   map[string]func() ([]byte, error)
}

// Warn adds a warning about the run as a whole, such as a verdict cache
// found unreadable as the run starts, to be reported with the first
// repository's result: under its warnings in JSON, below it in the text
// report. Call it before Run; it is lost if no repository runs.
func (r *Runner) Warn(msg string) {
	r.warnings = append(r.warnings, msg)
}

// DefaultParallel and DefaultReviewParallel are New's Parallel and
// ReviewParallel. Repositories are cheap to process at once, mostly reads
// with writes serialised anyway; each review is an agent process with a
// checkout of its own, spending tokens for minutes.
const (
	DefaultParallel       = 10
	DefaultReviewParallel = 4
)

func New(c github.Client, viewer string, apply bool) *Runner {
	return &Runner{Client: c, Viewer: viewer, Apply: apply, PollAttempts: 5, PollInterval: 3 * time.Second, Sleep: github.Sleep, Parallel: DefaultParallel, ReviewParallel: DefaultReviewParallel}
}

// Run processes up to Parallel repos at a time and returns their results in
// the order given, passing each to progress in that order as soon as it and
// those before it are done. A failure in one repo does not stop others; an
// interruption stops repos from starting, and those are left out.
// A Runner can be reused for successive runs. Run must not overlap another
// Run or an external RunRepo call on the same Runner.
func (r *Runner) Run(ctx context.Context, repos []config.Repo, progress func(RepoResult)) []RepoResult {
	// These belong to one run, not to the lifetime of a library client.
	// In particular, an owner's policy must be read again before any writes.
	r.orgFiles = nil
	r.writesBlocked.Store(false)
	r.reviewsStarted.Store(0)
	r.reviewsOnce = sync.Once{}
	r.reviewSlots = nil
	warnings := r.warnings
	r.warnings = nil

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
		if i == 0 && len(warnings) > 0 {
			results[0].Warnings = append(slices.Clone(warnings), results[0].Warnings...)
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

// errNotSent marks a write that never reached GitHub, so it cannot have
// gone through.
var errNotSent = errors.New("not sent")

// errWritesBlocked is a write not sent because an earlier one hit a rate
// limit.
var errWritesBlocked = fmt.Errorf("%w: an earlier write hit GitHub's rate limit; a later run tries again", errNotSent)

// interruptedInFlight reports whether the run was interrupted while the
// write that failed with err was on its way, so it may have gone through:
// it was sent, and GitHub did not answer that it refused it. A 500, 502,
// 503 or 504 is no such answer: the write may have gone through all the same.
func interruptedInFlight(ctx context.Context, err error) bool {
	return ctx.Err() != nil && !errors.Is(err, errNotSent) && !github.Refused(err)
}

// write makes one write at a time across repositories, once more after
// the pause GitHub asks for when it refuses it for a rate limit. The pause
// holds back the other repositories' writes too: the limit is the user's.
// A rate limit that is not waited out, or that refuses the write again,
// stops the run's later writes from being sent: writing on through it is
// what GitHub penalises.
func (r *Runner) write(ctx context.Context, f func() error) error {
	r.writes.Lock()
	defer r.writes.Unlock()
	if r.writesBlocked.Load() {
		return errWritesBlocked
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", errNotSent, err)
	}
	err := f()
	d, ok := github.RateLimited(err)
	if !ok || ctx.Err() != nil {
		return err
	}
	if d > github.MaxRateLimitWait {
		r.writesBlocked.Store(true)
		return github.NotWaiting(err)
	}
	if err := r.sleep(ctx, d); err != nil {
		return fmt.Errorf("%w: %w", errNotSent, err)
	}
	if err = f(); err == nil {
		return nil
	}
	if _, ok := github.RateLimited(err); !ok {
		return err
	}
	r.writesBlocked.Store(true)
	return fmt.Errorf("%w; rate-limited again after waiting %s; later writes not sent", err, d.Round(time.Second))
}

// sleep waits for d with Sleep, or with a timer when Sleep is not set, as
// in a Runner not made by New, unless ctx ends first.
func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep == nil {
		return github.Sleep(ctx, d)
	}
	return r.Sleep(ctx, d)
}

func (r *Runner) RunRepo(ctx context.Context, rc config.Repo) RepoResult {
	return r.RunRepoWithSetup(ctx, rc, nil)
}

// RunRepoWithSetup is RunRepo with setup called after policy files have
// been resolved, before any PR is evaluated. It lets a caller configure
// this runner's optional reviews using the effective repository policy.
// A setup error fails the result but still permits ordinary policy-only
// actions. Calls must not overlap other calls on this runner.
func (r *Runner) RunRepoWithSetup(ctx context.Context, rc config.Repo, setup func(config.Repo) error) RepoResult {
	rc, files, err := r.withFiles(ctx, rc)
	res := RepoResult{
		Repo:        rc.FullName(),
		Policy:      rc.Policy,
		ConfigFiles: files,
		Decisions:   []policy.Decision{},
		Findings:    []policy.Finding{},
		Events:      []Event{},
	}
	if err != nil {
		// Without the policy the repository asks for, nothing is judged.
		res.Error = errText(ctx, err)
		return res
	}
	var setupErr error
	if setup != nil {
		setupErr = setup(rc)
	}
	// A repository in report mode is only reported on, as in a dry run.
	apply := r.Apply && rc.Policy.Mode != config.ModeReport
	attempted := map[int]bool{}
	retried := map[int]bool{}
	rebased := map[int]bool{}
	// Remember successful review comments even before GitHub's next read
	// includes them. Across runs, the PR's own comments provide this record.
	commented := map[string]bool{}
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
	rv := &repoReviews{verdicts: map[string]runVerdict{}, moved: map[int]int{}, res: &res}
	// finish reports the merges still in unsure as the pass ends, such as
	// one whose PR became unmergeable for a while, so that none goes
	// unreported, and returns the result. Each is reported as GitHub says
	// it turned out, and failed when it cannot say.
	finish := func() RepoResult {
		if setupErr != nil {
			if res.Error != "" {
				res.Error += "; "
			}
			res.Error += setupErr.Error()
		}
		for _, n := range slices.Sorted(maps.Keys(unsure)) {
			p := unsure[n]
			if ctx.Err() != nil {
				// There is no asking GitHub now.
				switch {
				case p.uncertain:
					resolve(n, p.failed("interrupted; may have merged after "+p.cause))
				case p.failure != "":
					resolve(n, p.failed(p.failure))
				default:
					resolve(n, p.failed(p.cause))
				}
				continue
			}
			switch state, err := r.prState(ctx, rc, n); {
			case err != nil && p.uncertain:
				resolve(n, p.failed(fmt.Sprintf("may have merged after %s, but GitHub could not say: %s", p.cause, errText(ctx, err))))
			case err != nil && p.failure != "":
				resolve(n, p.failed(p.failure))
			case err != nil:
				// GitHub refused the merge: it did not go through.
				resolve(n, p.failed(p.cause))
			case state != github.PROpen:
				resolve(n, p.outcome(state))
			case p.failure != "":
				resolve(n, p.failed(p.failure))
			default:
				resolve(n, p.failed(fmt.Sprintf("not merged after %s, and not mergeable again in this run", p.cause)))
			}
		}
		// Decisions describe the latest snapshot. A PR closing, becoming a
		// draft or changing updates can remove its failed verdict from that
		// snapshot, but cannot turn a review that failed into a successful run.
		needsSummary := res.Error != ""
		seen := map[string]bool{}
		var failures []string
		for _, failure := range rv.failures {
			if !slices.ContainsFunc(res.Decisions, func(d policy.Decision) bool {
				return d.Number == failure.number && d.Verdict != nil && d.Verdict.Error == failure.message
			}) {
				needsSummary = true
			}
			msg := fmt.Sprintf("#%d: agent review failed: %s", failure.number, failure.message)
			if r.AgentName != "" {
				msg += " (agent: " + r.AgentName + ")"
			}
			if !seen[msg] {
				failures = append(failures, msg)
				seen[msg] = true
			}
		}
		if needsSummary && len(failures) > 0 {
			if res.Error != "" {
				failures = append([]string{res.Error}, failures...)
			}
			res.Error = strings.Join(failures, "; ")
		}
		return res
	}
	// fail ends the pass with err as the reason it could not go on.
	fail := func(err error) RepoResult {
		res.Error = errText(ctx, err)
		return finish()
	}
	polls := 0
	var judge policy.Judge
	if r.Reviewer != nil || r.Verdicts != nil {
		judge = func(d policy.Decision) *policy.Verdict {
			key := review.NewRequest(rc, d).Key()
			// As in the cache, a rejection holds only for the head it was
			// given on: Dependabot may have rebased onto a fix since.
			if v, ok := rv.verdicts[key]; ok && (v.Approved || v.head == d.PR.HeadOID) {
				return v.Verdict
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
			return fail(err)
		}
		settle(repo)
		res.Permission = repo.Permission
		method, methodErr := policy.MergeMethod(repo, rc.Policy.MergeMethod)
		res.MergeMethod = method
		refused := map[int]bool{}
		for _, ev := range res.Events {
			refused[ev.Number] = refused[ev.Number] || ev.Kind == EventMergeFailed || ev.Kind == EventRebaseFailed
		}
		decisions := policy.EvaluateAfter(repo, rc.Policy, r.Viewer, judge, refused)
		res.Decisions = decisions
		res.Findings = policy.Diagnose(repo, rc.Policy, decisions)
		if apply {
			for _, d := range decisions {
				if d.Verdict == nil {
					continue
				}
				body := r.reviewComment(rc, d)
				if commented[body] || slices.ContainsFunc(d.PR.Comments, func(c github.Comment) bool {
					return strings.EqualFold(c.Author, r.Viewer) && c.Body == body
				}) {
					continue
				}
				if err := r.comment(ctx, rc, d.Number, body); err != nil {
					// A timeout may hide a successful POST, but cannot prove
					// the explanation was published. Leave the PR unmerged;
					// the next run checks its comments before trying again.
					return fail(fmt.Errorf("#%d: post agent review comment: %w", d.Number, err))
				}
				commented[body] = true
			}
		}

		var next *policy.Decision
		rebaseRefused := false
		if apply {
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
				case interruptedInFlight(ctx, err):
					// It may have been posted, as above.
					ev.Detail = joinDetail(ev.Detail, "may not have been posted: interrupted")
					res.Warnings = append(res.Warnings, fmt.Sprintf("#%d: the rebase request may not have been posted: interrupted", d.Number))
				default:
					ev.Kind, ev.Detail = EventRebaseFailed, errText(ctx, err)
					rebaseRefused = true
				}
				res.Events = append(res.Events, ev)
			}

			for i := range decisions {
				if decisions[i].Action == policy.ActionMerge && !attempted[decisions[i].Number] {
					next = &decisions[i]
					break
				}
			}
		}

		if next == nil && rebaseRefused && ctx.Err() == nil && !r.writesBlocked.Load() {
			// The PR queued behind the refused one may ask in its place.
			continue
		}
		if next != nil {
			attempted[next.Number] = true
			// This try supersedes an earlier one that may have gone through:
			// it merges, or fails in a way that tells whether that one did.
			delete(unsure, next.Number)
			ev := Event{Number: next.Number, Title: next.Title, URL: next.URL, Grade: next.Grade, Kind: EventMerged}
			err = methodErr
			if err == nil {
				err = r.merge(ctx, rc, next.Number, method, next.PR.HeadOID)
			}
			// A merge the interruption cut short is reported below.
			uncertain := err != nil && github.Transient(err) && ctx.Err() == nil
			if uncertain && doubted[next.Number] == "" {
				doubted[next.Number] = err.Error()
			}
			if err != nil && (github.Retryable(err) || uncertain) && !retried[next.Number] && ctx.Err() == nil {
				// GitHub may not have caught up with the previous merge yet:
				// wait, refetch, and try this PR once more. A merge that timed
				// out may have gone through: if the PR is gone, it did.
				retried[next.Number] = true
				delete(attempted, next.Number)
				// Kept pending until it is tried again, so that a PR GitHub
				// shows unmergeable from now on is still reported failed.
				ev.Detail = joinDetail(method, r.approval(*next), "after "+err.Error())
				unsure[next.Number] = pendingMerge{merged: ev, cause: err.Error(), uncertain: uncertain}
				if err := r.sleep(ctx, r.PollInterval); err != nil {
					return fail(err)
				}
				continue
			}
			if cause := doubted[next.Number]; err != nil && cause != "" && ctx.Err() == nil {
				// One of the attempts may have merged the PR, and GitHub can
				// still show it open a moment later, so the retry's 405 or 409
				// may only mean it is already merged: read it again before
				// calling the merge failed.
				ev.Detail = joinDetail(method, r.approval(*next), "after "+cause)
				unsure[next.Number] = pendingMerge{merged: ev, cause: cause, failure: errText(ctx, err), uncertain: true}
				if err := r.sleep(ctx, r.PollInterval); err != nil {
					return fail(err)
				}
				continue
			}
			switch cause := doubted[next.Number]; {
			case err != nil && ctx.Err() != nil && cause != "":
				// An earlier attempt may have made it, and there is no
				// asking now.
				ev.Kind, ev.Detail = EventMergeFailed, "interrupted; may have merged after "+cause
			case err != nil && methodErr == nil && interruptedInFlight(ctx, err):
				// GitHub may have made it, and there is no asking now.
				ev.Kind, ev.Detail = EventMergeFailed, "interrupted; may have merged"
			case err != nil:
				ev.Kind, ev.Detail = EventMergeFailed, errText(ctx, err)
			default:
				ev.Detail = joinDetail(method, r.approval(*next))
				polls = 0
			}
			res.Events = append(res.Events, ev)
			if !r.writesBlocked.Load() {
				continue
			}
			// No write can be sent for the rest of the run: report this
			// pass's other merges as not sent, rather than read the
			// repository again for each, and go on to the reviews.
			for _, d := range decisions {
				if p, ok := unsure[d.Number]; ok {
					// Tried once already, and that try may have merged it:
					// reported as it turned out as the pass ends.
					p.failure = fmt.Sprintf("not merged after %s; retry %s", p.cause, errWritesBlocked)
					unsure[d.Number] = p
					continue
				}
				if d.Action == policy.ActionMerge && !attempted[d.Number] {
					attempted[d.Number] = true
					res.Events = append(res.Events, Event{Number: d.Number, Title: d.Title, URL: d.URL, Grade: d.Grade, Kind: EventMergeFailed, Detail: errWritesBlocked.Error()})
				}
			}
		}

		// Reviews come after merges: they take minutes, and the PRs that
		// need none should not wait on them.
		if r.reviewNext(ctx, rc, decisions, rv) {
			continue
		}
		// GitHub computes mergeability lazily: the first query after a push
		// or a merge often says UNKNOWN, and asking again resolves it.
		if !recomputing(decisions) || polls >= r.PollAttempts {
			return finish()
		}
		polls++
		if err := r.sleep(ctx, r.PollInterval); err != nil {
			return fail(err)
		}
	}
}

// pendingMerge is a merge GitHub failed in a way that may hide a success.
type pendingMerge struct {
	// merged is the event to report if the PR is no longer open.
	merged Event
	// cause is the failure that may hide a success.
	cause string
	// uncertain says cause leaves in doubt whether the merge went through,
	// as a 504 does; a 405 or 409 says it did not.
	uncertain bool
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
	return github.Again(ctx, r.PollInterval, r.sleep, func() (github.PRState, error) {
		state, err := r.Client.PullRequestState(ctx, rc.Owner, rc.Name, number)
		if err != nil {
			return state, err
		}
		// Custom clients must establish a known state too: an empty or
		// future value cannot prove an uncertain merge closed unmerged.
		switch state {
		case github.PROpen, github.PRMerged, github.PRClosed:
			return state, nil
		default:
			return "", fmt.Errorf("%s#%d: unrecognized pull request state %q", rc.FullName(), number, state)
		}
	})
}

// withFiles layers the policy files of rc's owner and of rc itself over
// its config, and names the files found.
func (r *Runner) withFiles(ctx context.Context, rc config.Repo) (config.Repo, []string, error) {
	org, err := r.orgFile(ctx, rc.Owner)
	if err != nil {
		return rc, nil, err
	}
	own, err := github.Again(ctx, r.PollInterval, r.sleep, func() ([]byte, error) {
		return r.Client.FileText(ctx, rc.Owner, rc.Name, config.RepoFile)
	})
	if err != nil {
		return rc, nil, err
	}
	var files []string
	if org != nil {
		files = append(files, rc.Owner+"/"+config.OrgRepo+":"+config.OrgFile)
	}
	if own != nil {
		files = append(files, config.RepoFile)
	}
	rc, err = rc.WithFiles(org, own)
	return rc, files, err
}

// orgFile reads owner's config.OrgFile, once per run however many of its
// repositories run.
func (r *Runner) orgFile(ctx context.Context, owner string) ([]byte, error) {
	key := strings.ToLower(owner)
	r.orgFilesMu.Lock()
	read, ok := r.orgFiles[key]
	if !ok {
		read = sync.OnceValues(func() ([]byte, error) {
			return github.Again(ctx, r.PollInterval, r.sleep, func() ([]byte, error) {
				return r.Client.FileText(ctx, owner, config.OrgRepo, config.OrgFile)
			})
		})
		if r.orgFiles == nil {
			r.orgFiles = map[string]func() ([]byte, error){}
		}
		r.orgFiles[key] = read
	}
	r.orgFilesMu.Unlock()
	return read()
}

// fetch reads the repository, once more when GitHub fails in a way that
// may pass.
func (r *Runner) fetch(ctx context.Context, rc config.Repo) (*github.Repo, error) {
	return github.Again(ctx, r.PollInterval, r.sleep, func() (*github.Repo, error) {
		return r.Client.FetchRepo(ctx, rc.Owner, rc.Name)
	})
}

// approval notes that the agent approved d, for the event acting on it.
func (r *Runner) approval(d policy.Decision) string {
	if d.Verdict == nil || !d.Verdict.Approved {
		return ""
	}
	name := d.Verdict.Agent
	if name == "" {
		name = r.AgentName
	}
	if name == "" {
		return "approved by agent"
	}
	return "approved by " + name
}

// joinDetail joins the non-empty parts of an event's detail.
func joinDetail(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), ", ")
}

// repoReviews is what one repository's pass knows about its reviews.
type repoReviews struct {
	// verdicts holds this run's reviews, failed ones included, so a PR is
	// reviewed at most once per head in a run.
	verdicts map[string]runVerdict
	// moved counts, by PR number, the reviews a PR's head moved under. It
	// is kept by number, not by key: the key of an update without versions
	// moves with its head, so each new head would look unreviewed.
	moved map[int]int
	res   *RepoResult
	// failures survive later snapshots and replacement verdicts.
	failures []reviewFailure
}

type reviewFailure struct {
	number  int
	message string
}

// runVerdict is a verdict given in this run, on the PR at head.
type runVerdict struct {
	*policy.Verdict
	head string
}

// headMoved is the note on a PR whose head kept moving under its review.
const headMoved = " (head moved during review; next run)"

// reviewNext has the agent review the first PR awaiting review and reports
// whether it did, after which the repository must be evaluated again. When
// the run's review cap is reached, it says so on the PRs left waiting.
func (r *Runner) reviewNext(ctx context.Context, rc config.Repo, decisions []policy.Decision, rv *repoReviews) bool {
	if r.Reviewer == nil {
		return false
	}
	for i := range decisions {
		d := &decisions[i]
		if d.Action != policy.ActionReview {
			continue
		}
		if rv.moved[d.Number] > 1 {
			d.Reason += headMoved
			continue
		}
		if !r.startReview() {
			for j := i; j < len(decisions); j++ {
				if decisions[j].Action == policy.ActionReview && rv.moved[decisions[j].Number] <= 1 {
					decisions[j].Reason += fmt.Sprintf(" (review limit of %d reached)", r.MaxReviews)
				}
			}
			return false
		}
		req := review.NewRequest(rc, *d)
		key := req.Key()
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
		}
		v.Agent = r.AgentName
		if err == nil && r.Verdicts != nil {
			r.verdictsMu.Lock()
			err := r.Verdicts.Put(key, req.HeadOID, r.AgentName, v)
			recovered := r.Verdicts.TakeRecovered()
			r.verdictsMu.Unlock()
			if err != nil {
				rv.res.Warnings = append(rv.res.Warnings, fmt.Sprintf("#%d: verdict not cached: %v", d.Number, err))
			}
			if recovered != "" {
				rv.res.Warnings = append(rv.res.Warnings, recovered)
			}
		}
		if v.Error != "" {
			rv.failures = append(rv.failures, reviewFailure{d.Number, v.Error})
		}
		rv.verdicts[key] = runVerdict{&v, req.HeadOID}
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
	// A ready slot can win the select even when cancellation is ready.
	if err := ctx.Err(); err != nil {
		return policy.Verdict{}, err
	}
	v, err := r.Reviewer.Review(ctx, req)
	// Custom reviewers may return an answer after cancellation. Never
	// persist that answer as an approval for a later run.
	if ctx.Err() != nil {
		return policy.Verdict{}, ctx.Err()
	}
	return v, err
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
		// The policy treats absent values as still being computed too.
		if d.Action == policy.ActionWait && (d.PR.Mergeable == "UNKNOWN" || d.PR.Mergeable == "" || d.PR.MergeState == "UNKNOWN" || d.PR.MergeState == "") {
			return true
		}
	}
	return false
}
