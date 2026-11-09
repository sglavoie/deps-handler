package pr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/github"
)

type mergeClient struct {
	github.MergeClient
	p                    github.PRSnapshot
	methods              []string
	calls                int
	method, head         string
	mergeErr, confirmErr error
	state                github.PRState
}

func (c *mergeClient) Snapshot(context.Context, github.PRTarget) (github.PRSnapshot, error) {
	return c.p, nil
}
func (c *mergeClient) MergeMethods(context.Context, string, string) ([]string, error) {
	return c.methods, nil
}
func (c *mergeClient) Merge(_ context.Context, _, _ string, _ int, method, head string) error {
	c.calls++
	c.method, c.head = method, head
	return c.mergeErr
}
func (c *mergeClient) PullRequestState(context.Context, string, string, int) (github.PRState, error) {
	return c.state, c.confirmErr
}

func TestMergeReadinessAndMethodSelection(t *testing.T) {
	for _, tc := range []struct {
		name           string
		change         func(*mergeClient)
		opts           MergeOptions
		status, method string
	}{
		{"preview", func(*mergeClient) {}, MergeOptions{}, "would_merge", "squash"},
		{"apply", func(*mergeClient) {}, MergeOptions{Apply: true}, "merged", "squash"},
		{"fallback", func(c *mergeClient) { c.methods = []string{"merge", "rebase"} }, MergeOptions{Apply: true}, "merged", "merge"},
		{"explicit", func(*mergeClient) {}, MergeOptions{Apply: true, MergeMethod: "rebase"}, "merged", "rebase"},
		{"disabled", func(c *mergeClient) { c.methods = []string{"squash"} }, MergeOptions{Apply: true, MergeMethod: "merge"}, "blocked", "merge"},
		{"no checks", func(c *mergeClient) { c.p.Checks = nil }, MergeOptions{Apply: true}, "merged", "squash"},
		{"incomplete checks", func(c *mergeClient) { c.p.Checks = nil; c.p.ChecksComplete = false }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"failed checks", func(c *mergeClient) { c.p.Checks[0].State = github.CheckFailed }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"pending checks", func(c *mergeClient) { c.p.Checks[0].State = github.CheckPending }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"review required", func(c *mergeClient) { c.p.ReviewDecision = "REVIEW_REQUIRED" }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"conflict", func(c *mergeClient) { c.p.Mergeable = "CONFLICTING" }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"unknown mergeability", func(c *mergeClient) { c.p.Mergeable = "UNKNOWN" }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"protected", func(c *mergeClient) { c.p.MergeState = "BLOCKED" }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"no permission", func(c *mergeClient) { c.p.Permission = "READ" }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"archived", func(c *mergeClient) { c.p.Archived = true }, MergeOptions{Apply: true}, "blocked", "squash"},
		{"draft", func(c *mergeClient) { c.p.Draft = true }, MergeOptions{Apply: true}, "skipped", ""},
		{"closed", func(c *mergeClient) { c.p.State = "CLOSED" }, MergeOptions{Apply: true}, "skipped", ""},
		{"merged", func(c *mergeClient) { c.p.State = "MERGED" }, MergeOptions{Apply: true}, "skipped", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &mergeClient{p: snapshot(github.CheckPassed), methods: []string{"squash", "merge", "rebase"}}
			tc.change(c)
			result, err := Merge(context.Background(), c, c.p.Target, tc.opts)
			if err != nil || result.Status != tc.status || result.MergeMethod != tc.method {
				t.Fatalf("%+v %v", result, err)
			}
			wantCalls := 0
			if tc.status == "merged" {
				wantCalls = 1
			}
			if c.calls != wantCalls || result.MergeAttempted != (wantCalls == 1) {
				t.Fatalf("calls=%d result=%+v", c.calls, result)
			}
			if wantCalls == 1 && (c.head != c.p.HeadOID || c.method != tc.method) {
				t.Fatal("incorrect merge request", c)
			}
		})
	}
}

func TestMergeReconcilesWithoutRetry(t *testing.T) {
	for _, state := range []github.PRState{github.PRMerged, github.PROpen, github.PRClosed} {
		c := &mergeClient{p: snapshot(github.CheckPassed), methods: []string{"squash"}, mergeErr: errors.New("lost response"), state: state}
		r, err := Merge(context.Background(), c, c.p.Target, MergeOptions{Apply: true})
		if c.calls != 1 || !r.MergeAttempted {
			t.Fatal(r, c.calls)
		}
		if state == github.PRMerged {
			if err != nil || r.Status != "merged" {
				t.Fatal(r, err)
			}
		} else if err == nil || r.Status != "error" || !strings.Contains(r.Error, "inspect GitHub before retrying") {
			t.Fatal(r, err)
		}
	}
}

func TestMergeRejectsIncompleteIdentityAndCancellation(t *testing.T) {
	for _, kind := range []string{"target", "head", "base", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			c := &mergeClient{p: snapshot(github.CheckPassed), methods: []string{"squash"}}
			target := c.p.Target
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "target":
				c.p.Target.Number++
			case "head":
				c.p.HeadOID = ""
			case "base":
				c.p.BaseOID = ""
			case "cancel":
				cancel()
			}
			r, err := Merge(ctx, c, target, MergeOptions{Apply: true})
			if err == nil || c.calls != 0 || r.Status != "error" {
				t.Fatal(r, err, c.calls)
			}
		})
	}
}
