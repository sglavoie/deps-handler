package cmd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sglavoie/deps-handler/internal/testutil"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/review"
)

// Observe both write paths even when a fixture contains a safe rebase.
type statusClient struct {
	*fakeClient
	writes atomic.Int32
}

func (c *statusClient) Merge(context.Context, string, string, int, string, string) error {
	c.writes.Add(1)
	return errors.New("status attempted a merge")
}

func (c *statusClient) Comment(context.Context, string, string, int, string) error {
	c.writes.Add(1)
	return errors.New("status attempted a comment")
}

func TestStatusSharesDryRunWithoutWritesOrReviews(t *testing.T) {
	behind := testutil.PR(3)
	behind.MergeState = "BEHIND"
	failing := testutil.PR(4)
	failing.Checks[0].State = github.CheckFailed
	fc := &statusClient{fakeClient: &fakeClient{repos: map[string]*github.Repo{
		"o/busy":   testutil.Repo(testutil.PR(1), testutil.PR(2, "lib:major"), failing),
		"o/behind": testutil.Repo(behind),
		"o/quiet":  testutil.Repo(),
	}}}
	oldClient, oldAgent := newClient, newAgent
	t.Cleanup(func() { newClient, newAgent = oldClient, oldAgent })
	newClient = func() (github.Client, error) { return fc, nil }
	newAgent = func(config.Agent, string) (review.Agent, error) {
		t.Error("status tried to create an agent")
		return nil, errors.New("no reviews")
	}
	cfg := writeConfig(t, "repos:\n  - o/busy\n  - o/behind\n  - o/quiet\n  - repo: o/skipped\n    skip: true\n")
	code, out, stderr := run("status", "-c", cfg)
	if code != 0 || stderr != "" || fc.writes.Load() != 0 {
		t.Fatalf("status = %d, %q, %q, writes %d", code, out, stderr, fc.writes.Load())
	}
	for _, want := range []string{
		"o/busy: 1 would merge / 0 waiting / 1 held / 1 failing",
		"o/behind: 0 would merge / 1 waiting / 0 held / 0 failing",
		"total: 1 would merge / 1 waiting / 1 held / 1 failing",
		"next: deps-handler dependabot -c " + cfg + " --apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "o/quiet") || strings.Contains(out, "o/skipped") {
		t.Fatal(out)
	}
	code, got, stderr := run("status", "-c", cfg, "--json")
	wantCode, want, _ := run("dependabot", "-c", cfg, "--open", "--json")
	if code != 0 || wantCode != 0 || got != want || stderr != "" || fc.writes.Load() != 0 {
		t.Fatalf("JSON differs from dry run: %d/%d, stderr %q\ngot %s\nwant %s", code, wantCode, stderr, got, want)
	}
}

func TestStatusErrorsAndFormats(t *testing.T) {
	_, cfg := runFixture(t)
	for _, args := range [][]string{{"--apply"}, {"--review"}, {"--agent", "claude"}, {"--json", "--list"}, {"--format", "bad"}, {"extra"}} {
		if code, _, stderr := run(append([]string{"status", "-c", cfg}, args...)...); code != 2 {
			t.Errorf("%v: code %d, %s", args, code, stderr)
		}
	}
	for _, args := range [][]string{{"--list"}, {"--format", "table"}, {"--format", "json"}} {
		if code, _, stderr := run(append([]string{"status", "-c", cfg}, args...)...); code != 0 {
			t.Errorf("%v: code %d, %s", args, code, stderr)
		}
	}
	cfg = writeConfig(t, "repos:\n  - o/missing\n")
	if code, out, _ := run("status", "-c", cfg); code != 1 || !strings.Contains(out, "o/missing: error:") || !strings.Contains(out, "1 repo failed") {
		t.Fatalf("read failure = %d, %s", code, out)
	}
}
