package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
)

type commandPRListClient struct {
	repos     []string
	listed    []string
	owner     string
	prs       map[string][]github.ListedPR
	repoErr   error
	prErr     error
	repoCalls int
}

func (c *commandPRListClient) ListRepositories(_ context.Context, owner string) ([]string, error) {
	c.repoCalls++
	c.owner = owner
	if owner != "" {
		filtered := []string{}
		for _, repo := range c.repos {
			if strings.HasPrefix(repo, owner+"/") {
				filtered = append(filtered, repo)
			}
		}
		return filtered, c.repoErr
	}
	return c.repos, c.repoErr
}

func (c *commandPRListClient) ListOpenPRs(_ context.Context, owner, name string) ([]github.ListedPR, error) {
	full := owner + "/" + name
	c.listed = append(c.listed, full)
	return c.prs[full], c.prErr
}

func mockPRListCommand(t *testing.T) *commandPRListClient {
	t.Helper()
	mockPRCommands(t)
	old := newPRListClient
	t.Cleanup(func() { newPRListClient = old })
	c := &commandPRListClient{repos: []string{"o/b", "p/a", "o/a"}, prs: map[string][]github.ListedPR{}}
	newPRListClient = func() (github.PRListClient, error) { return c, nil }
	return c
}

func TestPRListAcrossRepositoriesAndFormats(t *testing.T) {
	c := mockPRListCommand(t)
	created := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c.prs["o/b"] = []github.ListedPR{{Repository: "o/b", Number: 2, Title: "Later", URL: "https://github.com/o/b/pull/2", Author: "dependabot[bot]", Draft: true, CreatedAt: created.Add(time.Hour)}}
	c.prs["p/a"] = []github.ListedPR{{Repository: "p/a", Number: 1, Title: "Earlier", URL: "https://github.com/p/a/pull/1", Author: "dev", CreatedAt: created}}
	code, out, stderr := run("pr", "list", "--json")
	var doc struct {
		PullRequests []github.ListedPR `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err, out)
	}
	if code != 0 || stderr != "" || c.repoCalls != 1 || fmt.Sprint(c.listed) != "[o/a o/b p/a]" || len(doc.PullRequests) != 2 || doc.PullRequests[0].Repository != "p/a" || !doc.PullRequests[1].Draft {
		t.Fatal(code, out, stderr, c.listed)
	}
	code, out, stderr = run("pr", "list", "--list")
	if code != 0 || stderr != "" || !strings.Contains(out, "o/b#2") || !strings.Contains(out, "Earlier") || !strings.Contains(out, "https://github.com/p/a/pull/1") {
		t.Fatal(code, out, stderr)
	}
}

func TestPRListFiltersAndSingleOwner(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		owner string
		read  string
		calls int
	}{
		{[]string{"--owner", "o"}, "o", "[o/a o/b]", 1},
		{[]string{"--repo", "o/b", "--repo", "p/a"}, "", "[o/b p/a]", 0},
		{[]string{"--owner", "o", "--repo", "b", "--repo", "a"}, "", "[o/a o/b]", 0},
		{[]string{"--repo", "a"}, "", "[o/a p/a]", 1},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			c := mockPRListCommand(t)
			code, _, stderr := run(append([]string{"pr", "list", "--json"}, tc.args...)...)
			if code != 0 || stderr != "" || c.owner != tc.owner || fmt.Sprint(c.listed) != tc.read || c.repoCalls != tc.calls {
				t.Fatal(code, stderr, c.owner, c.listed, c.repoCalls)
			}
		})
	}
	for _, args := range [][]string{
		{"--owner", "o", "--owner", "p"}, {"--owner", ""}, {"--repo", ""},
		{"--repo", "o/b", "--repo", ""}, {"--owner", "o/p"},
		{"--owner", "o,p"},
		{"--owner", "o", "--repo", "p/a"}, {"--repo", "a/b/c"},
		{"--repo", "bad#name"}, {"extra"},
	} {
		t.Run("invalid "+strings.Join(args, " "), func(t *testing.T) {
			c := mockPRListCommand(t)
			code, _, _ := run(append([]string{"pr", "list"}, args...)...)
			if code != 2 || c.repoCalls != 0 || len(c.listed) != 0 {
				t.Fatal(code, c)
			}
		})
	}
}

func TestPRListErrorsDoNotPrintPartialResults(t *testing.T) {
	for _, repoError := range []bool{true, false} {
		c := mockPRListCommand(t)
		if repoError {
			c.repoErr = errors.New("repository listing failed")
		} else {
			c.prErr = errors.New("PR listing failed")
		}
		code, out, stderr := run("pr", "list", "--json")
		if code != 1 || out != "" || !strings.Contains(stderr, "failed") {
			t.Fatal(code, out, stderr)
		}
	}
}

func TestPRListUnknownBareRepositoryIsAnError(t *testing.T) {
	c := mockPRListCommand(t)
	code, out, stderr := run("pr", "list", "--repo", "missing", "--json")
	if code != 2 || out != "" || !strings.Contains(stderr, `no accessible repository matches --repo "missing"`) || c.repoCalls != 1 || len(c.listed) != 0 {
		t.Fatal(code, out, stderr, c)
	}
}

func TestPRListDemoCannotReachGitHub(t *testing.T) {
	c := mockPRListCommand(t)
	prCommandsAvailable = false
	code, _, stderr := run("pr", "list")
	if code != 1 || !strings.Contains(stderr, "unavailable in the demo") || c.repoCalls != 0 {
		t.Fatal(code, stderr)
	}
}
