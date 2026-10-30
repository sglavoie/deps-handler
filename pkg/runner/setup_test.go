package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/internal/testutil"
	"github.com/sglavoie/deps-handler/pkg/config"
)

func TestRepoSetupUsesResolvedPolicyOnce(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1), testutil.PR(2)), files: map[string]string{
		"o/.github:deps-handler.yml":   "agent_review: true\n",
		"o/r:.github/deps-handler.yml": "agent_review: false\n",
	}}
	calls := 0
	res := newRunner(c, true).RunRepoWithSetup(context.Background(), config.NewRepo("o", "r", config.Builtin), func(rc config.Repo) error {
		calls++
		if rc.Policy.AgentReview {
			t.Error("setup did not receive repository's opt out")
		}
		return nil
	})
	if res.Failed() || calls != 1 || !slices.Equal(c.merges, []int{1, 2}) {
		t.Fatalf("setup calls %d, merges %v, result %+v", calls, c.merges, res)
	}
}

func TestRepoSetupDoesNotRunWithoutValidPolicy(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1)), fileErr: errors.New("policy unreadable")}
	res := newRunner(c, true).RunRepoWithSetup(context.Background(), repoCfg, func(config.Repo) error {
		t.Fatal("setup ran without a resolved policy")
		return nil
	})
	if !res.Failed() || len(c.merges) != 0 {
		t.Fatalf("invalid policy did not stop the run: %+v", res)
	}
}

func TestRepoSetupErrorSurvivesLaterFetchFailure(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1)), fetchErrsAfterMerge: []error{errors.New("fetch failed")}}
	res := newRunner(c, true).RunRepoWithSetup(context.Background(), repoCfg, func(config.Repo) error {
		return errors.New("review setup failed")
	})
	if !res.Failed() || !strings.Contains(res.Error, "fetch failed") || !strings.Contains(res.Error, "review setup failed") || !slices.Equal(c.merges, []int{1}) {
		t.Fatalf("setup or fetch error lost: %+v; merges %v", res, c.merges)
	}
}
