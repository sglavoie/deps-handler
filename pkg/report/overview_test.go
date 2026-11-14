package report

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

func TestOverviewCounts(t *testing.T) {
	results := []runner.RepoResult{
		{Repo: "o/busy", Decisions: []policy.Decision{
			{Number: 1, Action: policy.ActionMerge},
			{Number: 2, Action: policy.ActionRebase},
			{Number: 3, Action: policy.ActionWait},
			{Number: 4, Action: policy.ActionSkip},
			{Number: 5, Action: policy.ActionSkip, FailedChecks: []string{"test", "lint"}},
		}},
		{Repo: "o/report", Policy: config.Policy{Mode: config.ModeReport}, Decisions: []policy.Decision{
			{Number: 1, Action: policy.ActionMerge},
			{Number: 2, Action: policy.ActionRebase},
		}},
		{Repo: "o/error", Error: "cannot read\x1b\nrepo", Decisions: []policy.Decision{{Number: 1, Action: policy.ActionMerge}}},
		{Repo: "o/warning", Warnings: []string{"incomplete\x1b"}},
	}
	var out bytes.Buffer
	if err := Overview(&out, results, 1, "deps-handler dependabot -c 'my config'"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"o/busy: 1 would merge / 2 waiting / 1 held / 1 failing",
		"o/report: 0 would merge / 0 waiting / 2 held / 0 failing",
		"o/error: error: cannot read repo",
		"warning: incomplete",
		"total: 1 would merge / 2 waiting / 3 held / 1 failing; 1 repo failed",
		"next: deps-handler dependabot -c 'my config' --apply",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("unsanitized output")
	}
}

func TestOverviewNothingToMerge(t *testing.T) {
	for _, results := range [][]runner.RepoResult{nil, {{Repo: "o/report", Policy: config.Policy{Mode: config.ModeReport}, Decisions: []policy.Decision{{Action: policy.ActionMerge}}}}} {
		var out bytes.Buffer
		if err := Overview(&out, results, 2, "deps-handler dependabot"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "next: deps-handler dependabot --open") || strings.Contains(out.String(), "--apply") {
			t.Fatal(out.String())
		}
		if results == nil && !strings.Contains(out.String(), "no open Dependabot PRs in 2 repos") {
			t.Fatal(out.String())
		}
	}
	if err := Overview(shortTableWriter{}, nil, 0, "deps-handler dependabot"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
}
