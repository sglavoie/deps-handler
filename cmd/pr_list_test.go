package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
)

type commandPRListClient struct {
	mu        sync.Mutex
	list      func(context.Context, string) ([]github.ListedPR, error)
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

func (c *commandPRListClient) ListOpenPRs(ctx context.Context, owner, name string) ([]github.ListedPR, error) {
	full := owner + "/" + name
	c.mu.Lock()
	c.listed = append(c.listed, full)
	c.mu.Unlock()
	if c.list != nil {
		return c.list(ctx, full)
	}
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
	if code != 0 || stderr != "" || c.repoCalls != 1 || fmt.Sprint(slices.Sorted(slices.Values(c.listed))) != "[o/a o/b p/a]" || len(doc.PullRequests) != 2 || doc.PullRequests[0].Repository != "p/a" || !doc.PullRequests[1].Draft {
		t.Fatal(code, out, stderr, c.listed)
	}
	code, out, stderr = run("pr", "list", "--list")
	if code != 0 || stderr != "" || !strings.Contains(out, "o/b#2") || !strings.Contains(out, "Earlier") || !strings.Contains(out, "https://github.com/p/a/pull/1") {
		t.Fatal(code, out, stderr)
	}
}

func TestPRListProgressCountsCompletedReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	release := make(chan struct{})
	c := &commandPRListClient{list: func(ctx context.Context, repository string) ([]github.ListedPR, error) {
		if repository == "o/slow" {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []github.ListedPR{}, nil
	}}
	counts := make(chan int, 2)
	done := make(chan error, 1)
	go func() {
		_, err := listPRRepositories(ctx, c, []string{"o/slow", "o/fast"}, func(n int) { counts <- n })
		done <- err
	}()
	select {
	case n := <-counts:
		if n != 1 {
			t.Error(n)
		}
	case <-ctx.Done():
		t.Error("completed repository did not report progress")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := <-counts; n != 2 {
		t.Fatal(n)
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
			if code != 0 || stderr != "" || c.owner != tc.owner || fmt.Sprint(slices.Sorted(slices.Values(c.listed))) != tc.read || c.repoCalls != tc.calls {
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

func TestPRListConcurrentReadsPreserveSort(t *testing.T) {
	c := mockPRListCommand(t)
	c.repos = nil
	for i := range 23 {
		c.repos = append(c.repos, fmt.Sprintf("o/r%02d", i))
	}
	var started, active, peak atomic.Int32
	ready := make(chan struct{})
	c.list = func(_ context.Context, repo string) ([]github.ListedPR, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		if started.Add(1) == 10 {
			close(ready)
		}
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			return nil, errors.New("listing did not run concurrently")
		}
		created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		return []github.ListedPR{
			{Repository: repo, Number: 3, CreatedAt: created},
			{Repository: repo, Number: 2, CreatedAt: created},
			{Repository: repo, Number: 1, CreatedAt: created.Add(-time.Hour)},
		}, nil
	}
	code, out, stderr := run("pr", "list", "--json")
	if code != 0 || stderr != "" || peak.Load() != 10 || active.Load() != 0 || started.Load() != 23 {
		t.Fatal(code, stderr, peak.Load(), active.Load(), started.Load())
	}
	var doc struct {
		PullRequests []github.ListedPR `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, numbers := range [][]int{{1}, {2, 3}} {
		for _, repo := range c.repos {
			for _, number := range numbers {
				want = append(want, fmt.Sprintf("%s#%d", repo, number))
			}
		}
	}
	var got []string
	for _, p := range doc.PullRequests {
		got = append(got, fmt.Sprintf("%s#%d", p.Repository, p.Number))
	}
	if !slices.Equal(got, want) {
		t.Fatal(got, want)
	}
}

func TestPRListConcurrentFailureCancelsAndJoinsWorkers(t *testing.T) {
	c := mockPRListCommand(t)
	c.repos = nil
	for i := range 25 {
		c.repos = append(c.repos, fmt.Sprintf("o/r%02d", i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var started, cancelled atomic.Int32
	ready := make(chan struct{})
	failure := errors.New("repository unavailable")
	c.list = func(ctx context.Context, repo string) ([]github.ListedPR, error) {
		if started.Add(1) == 10 {
			close(ready)
		}
		if repo == "o/r00" {
			// Fail only once every worker has started. Other workers observe
			// cancellation through one path, regardless of when they start waiting.
			select {
			case <-ready:
				return nil, failure
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		<-ctx.Done()
		cancelled.Add(1)
		return nil, ctx.Err()
	}
	prs, err := listPRRepositories(ctx, c, c.repos)
	if !errors.Is(err, failure) || prs != nil || started.Load() != 10 || cancelled.Load() != 9 {
		t.Fatal(prs, err, started.Load(), cancelled.Load())
	}
}

func TestPRListCancelledBeforeReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &commandPRListClient{}
	prs, err := listPRRepositories(ctx, c, []string{"o/a", "o/b"})
	if !errors.Is(err, context.Canceled) || prs != nil || len(c.listed) != 0 {
		t.Fatal(prs, err, c.listed)
	}
}

func TestPRListConcurrentFailureDiscardsSuccessfulReads(t *testing.T) {
	c := mockPRListCommand(t)
	success, waiting := make(chan struct{}), make(chan struct{})
	c.list = func(ctx context.Context, repo string) ([]github.ListedPR, error) {
		switch repo {
		case "o/a":
			close(success)
			return []github.ListedPR{{Repository: repo, Number: 1}}, nil
		case "p/a":
			close(waiting)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return nil, errors.New("listing did not cancel")
			}
		default:
			for _, ready := range []chan struct{}{success, waiting} {
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					return nil, errors.New("listing did not run concurrently")
				}
			}
			return nil, errors.New("repository unavailable")
		}
	}
	code, out, stderr := run("pr", "list", "--json")
	if code != 1 || out != "" || !strings.Contains(stderr, "repository unavailable") {
		t.Fatal(code, out, stderr)
	}
}

func TestPRListTableAndLegacyColumns(t *testing.T) {
	c := mockPRListCommand(t)
	t.Setenv("NO_COLOR", "1")
	c.prs["o/a"] = []github.ListedPR{{Repository: "o/a", Number: 1, Title: "New feature", Author: "dev", Draft: true, URL: "https://github.com/o/a/pull/1", CreatedAt: time.Now().Add(-3*24*time.Hour - time.Hour)}}
	code, out, stderr := run("pr", "list")
	if code != 0 || stderr != "" {
		t.Fatal(code, stderr)
	}
	for _, want := range []string{"New feature draft", "AGE", "3d", "URL", "https://github.com/o/a/pull/1"} {
		if !strings.Contains(out, want) {
			t.Fatal(want, out)
		}
	}
	if strings.Contains(out, "DRAFT") || strings.Contains(out, "CREATED") || strings.Contains(out, "\x1b") {
		t.Fatal(out)
	}
	code, out, stderr = run("pr", "list", "--list")
	if code != 0 || stderr != "" {
		t.Fatal(code, stderr)
	}
	for _, want := range []string{"DRAFT", "CREATED", "yes", c.prs["o/a"][0].CreatedAt.Local().Format("2006-01-02")} {
		if !strings.Contains(out, want) {
			t.Fatal(want, out)
		}
	}
	if strings.Contains(out, "AGE") || strings.Contains(out, "feature draft") {
		t.Fatal(out)
	}
}
