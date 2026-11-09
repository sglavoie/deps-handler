package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type commandMergeClient struct {
	*commandPRListClient
	*commandHandleClient
	merged   []github.PRTarget
	bases    []string
	failRepo string
}

func (c *commandMergeClient) Snapshot(ctx context.Context, t github.PRTarget) (github.PRSnapshot, error) {
	if t.Number == 11 {
		return github.PRSnapshot{}, errors.New("PR state unavailable")
	}
	s, err := c.HandleState(ctx, t)
	c.bases = append(c.bases, s.Snapshot.BaseOID)
	return s.Snapshot, err
}
func (*commandMergeClient) MergeMethods(context.Context, string, string) ([]string, error) {
	return []string{"squash"}, nil
}
func (c *commandMergeClient) Merge(ctx context.Context, owner, repo string, n int, method, head string) error {
	c.merged = append(c.merged, github.PRTarget{Owner: owner, Repo: repo, Number: n})
	return c.commandHandleClient.Merge(ctx, owner, repo, n, method, head)
}
func (c *commandMergeClient) ListOpenPRs(ctx context.Context, owner, repo string) ([]github.ListedPR, error) {
	if owner+"/"+repo == c.failRepo {
		return nil, errors.New("discovery failed")
	}
	return c.commandPRListClient.ListOpenPRs(ctx, owner, repo)
}
func mockMergeCommand(t *testing.T) *commandMergeClient {
	t.Helper()
	mockPRCommands(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := newPRMergeClient
	t.Cleanup(func() { newPRMergeClient = old })
	c := &commandMergeClient{
		commandPRListClient: &commandPRListClient{repos: []string{"o/b", "p/a", "o/a"}, prs: map[string][]github.ListedPR{}},
		commandHandleClient: &commandHandleClient{base: "initial"},
	}
	newPRMergeClient = func() (github.MergeClient, error) { return c, nil }
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
		t.Fatal("merge started an agent")
		return nil, nil
	}
	return c
}

func TestPRMergeRepositorySelection(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		listed string
		calls  int
	}{
		{nil, "[o/a o/b p/a]", 1},
		{[]string{"--owner", "o"}, "[o/a o/b]", 1},
		{[]string{"--repo", "o/b", "--repo", "p/a"}, "[o/b p/a]", 0},
		{[]string{"--owner", "o", "--repo", "a", "--repo", "b"}, "[o/a o/b]", 0},
		{[]string{"--repo", "a"}, "[o/a p/a]", 1},
		{[]string{"--repo", "o/b", "--repo", "O/B"}, "[o/b]", 0},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			c := mockMergeCommand(t)
			code, out, stderr := run(append([]string{"pr", "merge", "--json"}, tc.args...)...)
			if code != 0 || strings.TrimSpace(out) != "[]" || stderr != "" || fmt.Sprint(c.commandPRListClient.listed) != tc.listed || c.repoCalls != tc.calls {
				t.Fatal(code, out, stderr, c.commandPRListClient)
			}
		})
	}
}

func TestPRMergeAnyAuthorSerialApplyAndFormats(t *testing.T) {
	for _, format := range []string{"--json", "--list", "--table"} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprint(format, apply), func(t *testing.T) {
				c := mockMergeCommand(t)
				now := time.Now()
				c.prs["o/a"] = []github.ListedPR{{Repository: "o/a", Number: 7, Author: "dependabot[bot]", CreatedAt: now.Add(time.Hour)}}
				c.prs["p/a"] = []github.ListedPR{{Repository: "p/a", Number: 9, Author: "person", CreatedAt: now}, {Repository: "p/a", Number: 8, CreatedAt: now.Add(2 * time.Hour)}}
				args := []string{"pr", "merge", format}
				if apply {
					args = append(args, "--apply")
				}
				code, out, stderr := run(args...)
				wantCode := 0
				if apply {
					wantCode = 1
				}
				if code != wantCode || !strings.Contains(out, "blocked") {
					t.Fatal(code, out, stderr)
				}
				if format == "--json" {
					var results []pr.MergeResult
					if err := json.Unmarshal([]byte(out), &results); err != nil || len(results) != 3 || results[0].Target.Number != 9 || results[1].Target.Number != 7 {
						t.Fatal(out, err)
					}
				} else if !strings.Contains(out, "o/a#7") || !strings.Contains(out, "p/a#9") {
					t.Fatal(out)
				}
				if apply {
					if fmt.Sprint(c.merged) != "[p/a#9 o/a#7]" || fmt.Sprint(c.bases) != "[initial after9 after7]" {
						t.Fatal(c.merged, c.bases)
					}
				} else if len(c.merged) != 0 {
					t.Fatal(c.merged)
				}
			})
		}
	}
}

func TestPRMergeExplicitTargets(t *testing.T) {
	for _, args := range [][]string{
		{"other/project#7", "https://github.com/another/repo/pull/9"},
		{"7", "9", "--repo", "other/project"},
		{"7", "9", "--owner", "other", "-r", "project"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			c := mockMergeCommand(t)
			code, out, stderr := run(append([]string{"pr", "merge", "--apply", "--json"}, args...)...)
			if code != 0 || len(c.merged) != 2 || c.merged[0].Owner != "other" || c.merged[0].Repo != "project" || c.repoCalls != 0 || len(c.commandPRListClient.listed) != 0 {
				t.Fatal(code, out, stderr, c.merged)
			}
		})
	}
}

func TestPRMergeInvalidInputBeforeGitHub(t *testing.T) {
	for _, args := range [][]string{
		{"--owner", ""}, {"--repo", ""}, {"--owner", "o", "--owner", "p"},
		{"--owner", "o", "--repo", "p/a"}, {"--repo", "bad/repo/name"}, {"--owner", "bad,owner"},
		{"o/r#7", "--repo", "o/r"}, {"7"}, {"7", "--repo", "r"}, {"7", "--repo", "o/r", "--repo", "o/s"},
		{"o/r#7", "O/R#7"}, {"o/r#7", "--merge-method", "bad"}, {"--merge-method", ""},
		{"o/r#7", "--json", "--list"}, {"--repo", "o/r", "--repo", " "},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			mockMergeCommand(t)
			newPRMergeClient = func() (github.MergeClient, error) { t.Fatal("invalid input reached GitHub"); return nil, nil }
			code, _, stderr := run(append([]string{"pr", "merge"}, args...)...)
			if code != 2 {
				t.Fatal(code, stderr)
			}
		})
	}
}

func TestPRMergeDiscoveryFailurePreventsAllWrites(t *testing.T) {
	c := mockMergeCommand(t)
	c.prs["o/a"] = []github.ListedPR{{Repository: "o/a", Number: 7}}
	c.failRepo = "o/b"
	code, out, stderr := run("pr", "merge", "--apply", "--json")
	if code != 1 || out != "" || !strings.Contains(stderr, "discovery failed") || len(c.reads) != 0 || len(c.merged) != 0 {
		t.Fatal(code, out, stderr, c)
	}
}

func TestPRMergeContinuesAfterErrorsAndBlockers(t *testing.T) {
	c := mockMergeCommand(t)
	code, out, stderr := run("pr", "merge", "o/r#11", "o/r#8", "o/r#7", "--apply", "--json")
	var results []pr.MergeResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatal(out, err)
	}
	if code != 1 || len(results) != 3 || results[0].Error != "PR state unavailable" || results[1].Status != "blocked" || results[2].Status != "merged" || fmt.Sprint(c.merged) != "[o/r#7]" {
		t.Fatal(code, out, stderr, c.merged)
	}
}

func TestPRMergeDemoCannotReachGitHub(t *testing.T) {
	mockMergeCommand(t)
	prCommandsAvailable = false
	newPRMergeClient = func() (github.MergeClient, error) { t.Fatal("demo reached GitHub"); return nil, nil }
	code, _, stderr := run("pr", "merge", "o/r#7", "--apply")
	if code != 1 || !strings.Contains(stderr, "unavailable in the demo") {
		t.Fatal(code, stderr)
	}
}
