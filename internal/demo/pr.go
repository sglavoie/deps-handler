package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
)

// PRClient is a separate fictional scenario for general PR commands. All writes
// change memory only; even unknown targets and unsupported operations stay offline.
type PRClient struct {
	mu      sync.Mutex
	states  map[github.PRTarget]github.HandleState
	created time.Time
}

func NewPRClient() *PRClient {
	c := &PRClient{states: make(map[github.PRTarget]github.HandleState), created: time.Now().Add(-72 * time.Hour)}
	for i, title := range []string{"Clarify checkout confirmation", "Fix empty cart total", "Choose guest checkout policy"} {
		target := github.PRTarget{Owner: "acme", Repo: "storefront", Number: 430 + i}
		check, mergeState, decision := github.CheckPassed, "CLEAN", "APPROVED"
		if i == 1 {
			check, mergeState = github.CheckFailed, "UNSTABLE"
		}
		if i == 2 {
			mergeState, decision = "BLOCKED", "REVIEW_REQUIRED"
		}
		zero := 0
		c.states[target] = github.HandleState{
			Snapshot: github.PRSnapshot{Target: target, Title: title, URL: fmt.Sprintf("https://github.com/acme/storefront/pull/%d", target.Number), State: "OPEN", Permission: "WRITE", HeadOID: fmt.Sprintf("demo-head-%d", target.Number), HeadRef: fmt.Sprintf("change-%d", target.Number), HeadRepo: "acme/storefront", BaseRef: "main", BaseOID: "demo-base", Mergeable: "MERGEABLE", MergeState: mergeState, ReviewDecision: decision, ChecksComplete: true, Checks: []github.PRCheck{{Check: github.Check{Name: "test", State: check}}}},
			Author:   "octocat", UpdatedAt: "2026-11-14T12:00:00Z", Body: title, MergeMethods: []string{"squash"}, DiscussionsComplete: true,
			Comments: github.DiscussionPage{TotalCount: &zero, Nodes: []github.DiscussionComment{}}, Reviews: github.DiscussionPage{TotalCount: &zero, Nodes: []github.DiscussionComment{}}, Threads: []github.DiscussionThread{},
		}
	}
	return c
}

func (*PRClient) ListRepositories(ctx context.Context, owner string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner != "" && !strings.EqualFold(owner, "acme") {
		return []string{}, nil
	}
	return []string{"acme/storefront"}, nil
}

func (c *PRClient) ListOpenPRs(ctx context.Context, owner, name string) ([]github.ListedPR, error) {
	if !pause(ctx, 250*time.Millisecond) {
		return nil, ctx.Err()
	}
	if !strings.EqualFold(owner+"/"+name, "acme/storefront") {
		return nil, errors.New("demo PR repository not found: use acme/storefront")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []github.ListedPR{}
	for n := 430; n <= 432; n++ {
		s := c.states[github.PRTarget{Owner: "acme", Repo: "storefront", Number: n}]
		if s.Snapshot.State == "OPEN" {
			out = append(out, github.ListedPR{Repository: "acme/storefront", Number: n, Title: s.Snapshot.Title, URL: s.Snapshot.URL, Author: s.Author, CreatedAt: c.created.Add(time.Duration(n-430) * time.Hour)})
		}
	}
	return out, nil
}

func (c *PRClient) OpenPRs(ctx context.Context, owner, name string) ([]github.PRTarget, error) {
	listed, err := c.ListOpenPRs(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	out := []github.PRTarget{}
	for _, p := range listed {
		out = append(out, github.PRTarget{Owner: owner, Repo: name, Number: p.Number})
	}
	return out, nil
}

func canonical(t github.PRTarget) github.PRTarget {
	t.Owner, t.Repo = strings.ToLower(t.Owner), strings.ToLower(t.Repo)
	return t
}

func (c *PRClient) HandleState(ctx context.Context, target github.PRTarget) (github.HandleState, error) {
	if err := ctx.Err(); err != nil {
		return github.HandleState{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.states[canonical(target)]
	if !ok {
		return s, fmt.Errorf("demo PR %s not found: use acme/storefront#430, #431 or #432", target)
	}
	s.Snapshot.Target = target
	s.Snapshot.Checks = append([]github.PRCheck(nil), s.Snapshot.Checks...)
	return s, nil
}

func (c *PRClient) Snapshot(ctx context.Context, target github.PRTarget) (github.PRSnapshot, error) {
	s, err := c.HandleState(ctx, target)
	return s.Snapshot, err
}

func (c *PRClient) Evidence(ctx context.Context, p github.PRSnapshot) (github.PREvidence, error) {
	s, err := c.HandleState(ctx, p.Target)
	if err != nil {
		return github.PREvidence{}, err
	}
	return github.PREvidence{Details: &github.PullRequestDetails{Body: s.Body, Diff: "Fictional demo change: " + p.Title}, Checks: p.Checks, Base: github.BaseChecks{OID: p.BaseOID, ChecksComplete: true, Checks: []github.PRCheck{{Check: github.Check{Name: "test", State: github.CheckPassed}}}}}, nil
}

func (*PRClient) MergeMethods(ctx context.Context, owner, name string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !strings.EqualFold(owner+"/"+name, "acme/storefront") {
		return nil, errors.New("demo PR repository not found: use acme/storefront")
	}
	return []string{"squash"}, nil
}

func (c *PRClient) Merge(ctx context.Context, owner, name string, number int, method, head string) error {
	if !pause(ctx, 250*time.Millisecond) {
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	target := canonical(github.PRTarget{Owner: owner, Repo: name, Number: number})
	s, ok := c.states[target]
	if !ok || s.Snapshot.State != "OPEN" || s.Snapshot.HeadOID != head || s.Snapshot.MergeState != "CLEAN" || method != "squash" {
		return errors.New("demo merge requires an open, clean PR and its current head")
	}
	s.Snapshot.State = "MERGED"
	c.states[target] = s
	for t, next := range c.states {
		next.Snapshot.BaseOID = fmt.Sprintf("demo-base-after-%d", number)
		c.states[t] = next
	}
	return nil
}

func (c *PRClient) PullRequestState(ctx context.Context, owner, name string, number int) (github.PRState, error) {
	p, err := c.Snapshot(ctx, github.PRTarget{Owner: owner, Repo: name, Number: number})
	if err != nil {
		return "", err
	}
	if p.State == "MERGED" {
		return github.PRMerged, nil
	}
	return github.PROpen, nil
}

var errDemoRerun = errors.New("CI reruns are unavailable in the demo build; try pr list, pr merge or pr handle")

func (*PRClient) ActionsJob(context.Context, github.PRTarget, int64) (github.ActionsJob, error) {
	return github.ActionsJob{}, errDemoRerun
}
func (*PRClient) ActionsRun(context.Context, github.PRTarget, int64) (github.ActionsRun, error) {
	return github.ActionsRun{}, errDemoRerun
}
func (*PRClient) RerunJob(context.Context, github.PRTarget, int64) error        { return errDemoRerun }
func (*PRClient) RerunFailedJobs(context.Context, github.PRTarget, int64) error { return errDemoRerun }

// PRWorkspace creates only an empty demo directory. No git or agent subprocess
// runs; publishing a canned repair changes the fictional head and checks.
type PRWorkspace struct {
	Client *PRClient
	Root   string
	target github.PRTarget
}

func (w *PRWorkspace) Checkout(ctx context.Context, owner, name string, number int, head string) (string, error) {
	w.target = github.PRTarget{Owner: owner, Repo: name, Number: number}
	p, err := w.Client.Snapshot(ctx, w.target)
	if err != nil {
		return "", err
	}
	if p.HeadOID != head {
		return "", errors.New("demo checkout head changed")
	}
	return w.Root, os.MkdirAll(w.Root, 0o700)
}
func (*PRWorkspace) FetchBase(ctx context.Context, _, _ string) error { return ctx.Err() }
func (*PRWorkspace) Commit(context.Context, string, string) (string, error) {
	return "", errors.New("demo only supports repairs through pr handle --apply")
}
func (*PRWorkspace) DiffSummary(context.Context, string, string) (string, error) {
	return "", errors.New("demo has no real diff")
}
func (w *PRWorkspace) CommitAndPush(ctx context.Context, dir, owner, name, branch, expected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	w.Client.mu.Lock()
	defer w.Client.mu.Unlock()
	t := canonical(w.target)
	s := w.Client.states[t]
	if t.Number != 431 || dir != w.Root || !strings.EqualFold(owner+"/"+name, s.Snapshot.HeadRepo) || branch != s.Snapshot.HeadRef || expected != s.Snapshot.HeadOID {
		return "", errors.New("unexpected demo repair target")
	}
	s.Snapshot.HeadOID, s.Snapshot.MergeState, s.UpdatedAt = "demo-repaired-431", "CLEAN", "2026-11-14T12:01:00Z"
	s.Snapshot.Checks = []github.PRCheck{{Check: github.Check{Name: "test", State: github.CheckPassed}}}
	w.Client.states[t] = s
	return s.Snapshot.HeadOID, nil
}

// TaskAgent returns schema-valid canned answers, never launching a real model.
type TaskAgent struct{ AgentName string }

func (a TaskAgent) Name() string { return a.AgentName }
func (TaskAgent) Task(ctx context.Context, _, prompt, _ string, writable bool) (json.RawMessage, error) {
	if !pause(ctx, 350*time.Millisecond) {
		return nil, ctx.Err()
	}
	_, data, ok := strings.Cut(prompt, "Evidence (JSON):\n")
	if !ok {
		return nil, errors.New("demo agent requires PR evidence")
	}
	var evidence struct {
		PR       github.HandleState `json:"pr"`
		Snapshot github.PRSnapshot  `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(data), &evidence); err != nil {
		return nil, err
	}
	p := evidence.PR.Snapshot
	if writable {
		p = evidence.Snapshot
	}
	if canonical(p.Target).Owner != "acme" || canonical(p.Target).Repo != "storefront" || p.Target.Number < 430 || p.Target.Number > 432 {
		return nil, errors.New("unknown demo agent target")
	}
	a := pr.Analysis{Summary: "Checkout confirmation is clear; no code concerns found.", Causes: []string{}, Steps: []string{}, Validation: []string{}, Remaining: []string{}, Recommendations: []pr.Recommendation{}}
	decision := "approve"
	if writable {
		if p.Target.Number != 431 {
			return nil, errors.New("unexpected writable demo task")
		}
		a.Summary, a.ValidationStatus = "Fixed empty cart total (simulated repair).", "passed"
		a.Steps = []string{"Return zero for an empty cart in src/cart.ts."}
		a.Validation = []string{"npm test -- cart: passed (simulated)"}
		return json.Marshal(a)
	}
	switch p.Target.Number {
	case 431:
		if p.HeadOID == "demo-repaired-431" {
			a.Summary = "Re-reviewed the repaired head; empty carts now total zero."
		} else {
			decision, a.Summary = "repair", "Empty carts throw instead of returning zero."
			a.Recommendations = []pr.Recommendation{{Subject: "test", Action: pr.CodeRepair, Reason: "src/cart.ts reduces an empty array without an initial value.", NextStep: "Start the sum at zero and add an empty-cart regression test."}}
		}
	case 432:
		decision, a.Summary = "blocked", "Guest checkout requirements need a decision."
		a.Recommendations = []pr.Recommendation{{Subject: "checkout policy", Action: pr.HumanAction, Reason: "The PR does not specify whether guests may complete checkout.", NextStep: "Should guests be allowed to check out, or must they sign in?"}}
	}
	return json.Marshal(pr.HandleReview{Decision: decision, Analysis: a})
}
