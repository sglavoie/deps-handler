package output

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sglavoie/deps-merger/internal/ghfake"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/runner"
)

const login = "dmc[bot]"

func fake() *ghfake.Issues { return &ghfake.Issues{Repo: "o/r", Login: login} }

func result(prs int) runner.RepoResult {
	p := config.Builtin
	p.Mode, p.DependabotCommands = config.ModeReport, false
	res := runner.RepoResult{Repo: "o/r", Policy: p, Decisions: []policy.Decision{}}
	for n := 1; n <= prs; n++ {
		res.Decisions = append(res.Decisions, policy.Decision{Number: n, Title: "bump lib", Grade: config.UpdatePatch, Action: policy.ActionMerge, Reason: "checks green"})
	}
	return res
}

func TestSyncCreatesUpdatesClosesAndReopens(t *testing.T) {
	f := fake()
	c, ctx := f.Client(t), context.Background()

	// Nothing to report and no issue: nothing is written.
	if n, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(result(0), false), Wanted(result(0)), true); err != nil || n != 0 || ch != Unchanged {
		t.Fatalf("quiet: %d %q %v", n, ch, err)
	}

	n, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(result(1), false), true, true)
	if err != nil || n != 1 || ch != Created || f.Pins() != 1 {
		t.Fatalf("create: %d %q %v, pins %d", n, ch, err, f.Pins())
	}
	if body := f.Get(1).Body; !strings.Contains(body, Marker) || !strings.Contains(body, "| would merge |") {
		t.Fatalf("body:\n%s", body)
	}

	// The same report again: not even an edit.
	f.TakeCalls()
	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(result(1), false), true, true); err != nil || ch != Unchanged {
		t.Fatalf("same: %q %v", ch, err)
	}
	for _, call := range f.TakeCalls() {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("unchanged report wrote: %v", call)
		}
	}

	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(result(2), false), true, true); err != nil || ch != Updated {
		t.Fatalf("update: %q %v", ch, err)
	}
	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(result(0), false), false, true); err != nil || ch != Closed || f.Get(1).State != "closed" {
		t.Fatalf("close: %q %v, state %s", ch, err, f.Get(1).State)
	}
	// Found again by its marker once the recorded number is lost.
	if m, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(result(1), false), true, true); err != nil || m != n || ch != Reopened || f.Len() != 1 {
		t.Fatalf("reopen: %d %q %v, %d issues", m, ch, err, f.Len())
	}
}

func TestSyncLeavesOtherIssuesAlone(t *testing.T) {
	f := fake()
	// Someone copied the marker into an issue of their own, and the App
	// once opened an issue without it.
	f.Add("mallory", Title, Marker+"\nfake", "open")
	f.Add(login, Title, "something else", "open")
	n, ch, err := Sync(context.Background(), f.Client(t), login, "o", "r", 1, Body(result(1), false), true, true)
	if err != nil || ch != Created || n != 3 {
		t.Fatalf("got %d %q %v", n, ch, err)
	}
	if f.Get(1).Body != Marker+"\nfake" || f.Get(2).Body != "something else" {
		t.Fatal("another issue was edited")
	}
}

func TestBodyEscapesAndIsStable(t *testing.T) {
	res := result(1)
	res.Decisions[0].Title = "bump @octocat | lib"
	a, b := Body(res, false), Body(res, false)
	if a != b {
		t.Fatal("body differs between renders of the same result")
	}
	if strings.Contains(a, "@octocat") || !strings.Contains(a, `\|`) {
		t.Fatalf("body not escaped:\n%s", a)
	}
}

func TestBodyBoundsLargeReports(t *testing.T) {
	for name, text := range map[string]string{
		"ASCII":   strings.Repeat("long reason ", 30_000),
		"Unicode": strings.Repeat("更新が必要です。", 30_000),
	} {
		t.Run(name, func(t *testing.T) {
			res := result(2)
			res.Decisions[0].URL = "https://github.com/o/r/pull/1"
			res.Decisions[1].Reason = text
			body := Body(res, false)
			if len(body) > 60_000 || !utf8.ValidString(body) {
				t.Fatalf("issue body must remain publishable: %d bytes, valid UTF-8: %v", len(body), utf8.ValidString(body))
			}
			if !strings.HasPrefix(body, Marker+"\n") || !strings.Contains(body, "#1 bump lib") {
				t.Fatal("truncation discarded the issue marker or the preceding PR")
			}
			if !strings.Contains(body, "Report truncated") {
				t.Fatal("large report was cut off without telling the reader")
			}
			if body != Body(res, false) {
				t.Fatal("truncated body differs between renders of the same result")
			}
		})
	}
	if body := Body(result(2), false); strings.Contains(body, "Report truncated") {
		t.Fatal("ordinary report was marked as truncated")
	}
}

func TestBodySaysWhetherTheAppMerges(t *testing.T) {
	report := result(1)
	merge := result(1)
	merge.Policy.Mode = config.ModeMerge
	for _, c := range []struct {
		name    string
		res     runner.RepoResult
		merging bool
		want    []string
	}{
		{"report mode", report, false, []string{"report mode", "add `mode: merge`", "| would merge |"}},
		{"report mode on a merging deployment", report, true, []string{"report mode", "| would merge |"}},
		{"merge mode", merge, true, []string{"merges the Dependabot PRs it finds safe", "| merge |"}},
		{"merge mode, merging off", merge, false, []string{"`mode: merge` is set, but merging is paused or not enabled", "| would merge |"}},
	} {
		body := Body(c.res, c.merging)
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q in:\n%s", c.name, w, body)
			}
		}
	}
}

func TestSyncDoesNotOpenAnIssueForAFailure(t *testing.T) {
	f := fake()
	c, ctx := f.Client(t), context.Background()
	res := result(0)
	res.Error = "GraphQL: Resource not accessible by integration"
	if n, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(res, false), Wanted(res), false); err != nil || n != 0 || ch != Unchanged || f.Len() != 0 {
		t.Fatalf("got %d %q %v, %d issues", n, ch, err, f.Len())
	}
	// An existing issue does show the failure.
	n, _, _ := Sync(ctx, c, login, "o", "r", 0, Body(result(1), false), true, true)
	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(res, false), Wanted(res), false); err != nil || ch != Updated || !strings.Contains(f.Get(1).Body, "Resource not accessible") {
		t.Fatalf("update: %q %v", ch, err)
	}
}

func TestSyncWithIssuesDisabled(t *testing.T) {
	f := fake()
	f.Disable()
	if _, _, err := Sync(context.Background(), f.Client(t), login, "o", "r", 0, Body(result(1), false), true, true); !errors.Is(err, ErrIssuesDisabled) {
		t.Fatalf("err = %v", err)
	}
}
