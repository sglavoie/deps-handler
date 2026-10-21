//go:build demo

package cmd

import (
	"os"
	"path/filepath"
	"time"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/demo"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/review"
)

// The demo build talks to the make-believe GitHub of package demo instead
// of the real one, and keeps its config, verdicts and checkouts in a
// directory of its own, so it can neither touch a repository nor the
// state of a real install.
func init() {
	Version += "+demo"
	dir := filepath.Join(os.TempDir(), "deps-merger-demo")
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		os.Setenv(v, dir)
	}
	// No real token is read, even to be ignored.
	os.Setenv("GH_TOKEN", "demo")
	if err := os.MkdirAll(filepath.Join(dir, "deps-merger"), 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "deps-merger", "config.yaml"), []byte(demo.Config), 0o600)
	}
	client := demo.NewClient()
	newClient = func() (github.Client, error) { return client, nil }
	newAgent = func(config.Agent, string) (review.Agent, error) { return demo.Agent{}, nil }
	newReviewer = func(review.Agent, github.Client, *review.Workspace, time.Duration) review.Reviewer {
		return demo.Reviewer{}
	}
}
