//go:build demo

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
)

// The demo recorded for the README merges, rebases, and has the agent
// approve and reject, all against the make-believe GitHub.
func TestDemo(t *testing.T) {
	os.RemoveAll(filepath.Join(os.Getenv("XDG_STATE_HOME"), "deps-handler", "verdicts.json"))
	code, out, errOut := run("dependabot", "--review", "--apply")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"✓ merged", "↻ rebase-requested", "lodash 4.17.20 → 4.17.21", "done in ", "approved by claude", "rejected by claude", `check "lint" fails`} {
		if !strings.Contains(out, want) {
			t.Errorf("demo output lacks %q:\n%s", want, out)
		}
	}
}

// Exercise the production command/service paths with the demo factories intact.
// No executable or HTTP request is available as a fallback for these doubles.
func TestDemoPRCommandsOffline(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	oldTransport := http.DefaultTransport
	http.DefaultTransport = demoNoNetwork{t}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	code, out, stderr := run("pr", "list", "--json")
	var listing struct {
		PullRequests []github.ListedPR `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(out), &listing); err != nil || code != 0 || len(listing.PullRequests) != 3 {
		t.Fatalf("list: code=%d out=%s stderr=%s err=%v", code, out, stderr, err)
	}
	for _, p := range listing.PullRequests {
		if github.DependabotAuthor(p.Author) {
			t.Fatal("expected general PR fixture", p)
		}
	}

	for _, apply := range []bool{false, true} {
		args := []string{"pr", "merge", "--json"}
		if apply {
			args = append(args, "--apply")
		}
		code, out, stderr = run(args...)
		wantCode, wantStatus := 0, "would_merge"
		if apply {
			wantCode, wantStatus = 1, "merged"
		}
		var results []pr.MergeResult
		if err := json.Unmarshal([]byte(out), &results); err != nil || code != wantCode || len(results) != 3 {
			t.Fatalf("merge apply=%t: code=%d out=%s stderr=%s err=%v", apply, code, out, stderr, err)
		}
		if results[0].Status != wantStatus || results[0].MergeAttempted != apply || results[1].Status != "blocked" || results[2].Status != "blocked" {
			t.Fatal(out)
		}

		args = []string{"pr", "handle", "--repo", "acme/storefront", "--agent", "claude", "--json"}
		if apply {
			args = append(args, "--apply")
		}
		code, out, stderr = run(args...)
		var handled []pr.HandleResult
		if err := json.Unmarshal([]byte(out), &handled); err != nil || code != wantCode || len(handled) != 3 {
			t.Fatalf("handle apply=%t: code=%d out=%s stderr=%s err=%v", apply, code, out, stderr, err)
		}
		for _, result := range handled {
			if result.Workspace != "" {
				t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(result.Workspace)) })
			}
			if result.Error != "" {
				t.Fatal(out)
			}
		}
		wantRepair := "would_repair"
		if apply {
			wantRepair = "merged"
		}
		if handled[0].Status != wantStatus || handled[1].Status != wantRepair || handled[2].Status != "blocked" || !strings.Contains(out, "Should guests be allowed to check out") {
			t.Fatal(out)
		}
		if apply {
			r := handled[1]
			if len(r.Commits) != 1 || len(r.Repairs) != 1 || r.Repairs[0].ValidationStatus != "passed" || len(r.Reviews) != 2 || r.Reviews[1].Decision != "approve" || r.State.Snapshot.HeadOID != r.Commits[0] || !r.MergeAttempted {
				t.Fatal("repair was not published, checked and re-reviewed", out)
			}
			if handled[2].MergeAttempted || len(handled[2].Commits) != 0 {
				t.Fatal("blocked PR was changed", out)
			}
		} else {
			for _, result := range handled {
				if result.MergeAttempted || len(result.Commits) != 0 || len(result.Repairs) != 0 {
					t.Fatal("preview performed a write", out)
				}
			}
		}
	}
	for _, args := range [][]string{{"pr", "list", "--repo", "real/repo"}, {"pr", "merge", "real/repo#1", "--apply"}, {"pr", "handle", "real/repo#1", "--apply"}} {
		code, out, stderr := run(args...)
		if code != 1 || !strings.Contains(out+stderr, "demo PR") {
			t.Fatal(args, code, stderr)
		}
	}
}

type demoNoNetwork struct{ t *testing.T }

func (d demoNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	d.t.Error("demo attempted an HTTP request")
	return nil, errors.New("network disabled in demo test")
}

func TestDemoUnsupportedPRCommandsNameAvailableCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	oldClient, oldRerun, oldTask, oldLocal := newPRClient, newPRRerunClient, newTaskAgent, newLocalPRWorkspace
	t.Cleanup(func() {
		newPRClient, newPRRerunClient, newTaskAgent, newLocalPRWorkspace = oldClient, oldRerun, oldTask, oldLocal
	})
	newPRClient = func() (github.PRClient, error) { t.Fatal("unsupported command created GitHub client"); return nil, nil }
	newPRRerunClient = func() (github.ActionsClient, error) {
		t.Fatal("unsupported command created Actions client")
		return nil, nil
	}
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
		t.Fatal("unsupported command created agent")
		return nil, nil
	}
	newLocalPRWorkspace = func(context.Context, string) (pr.Workspace, func(), error) {
		t.Fatal("unsupported command accessed local checkout")
		return nil, nil, nil
	}
	for _, args := range [][]string{
		{"pr", "diagnose", "acme/storefront#431"},
		{"pr", "fix", "acme/storefront#431", "--push", "--working-directory", t.TempDir()},
		{"pr", "rerun", "acme/storefront#431", "--apply"},
		{"pr", "eval", "lyzma"},
	} {
		code, _, stderr := run(args...)
		if code != 1 || !strings.Contains(stderr, "unavailable in the demo build; try pr list, pr merge or pr handle") {
			t.Fatal(args, code, stderr)
		}
	}
	// Listing embedded evaluation cases already works without an agent.
	code, out, stderr := run("pr", "eval", "--list")
	if code != 0 || !strings.Contains(out, "lyzma") {
		t.Fatal(code, out, stderr)
	}
}
