//go:build demo

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/sglavoie/deps-merger/internal/demo"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/review"
)

// The demo build talks to the make-believe GitHub of package demo instead
// of the real one, and keeps its config, verdicts and checkouts in a
// directory of its own, so it can neither touch a repository nor the
// state of a real install.
func init() {
	Version += "+demo"
	prCommandsAvailable = false
	dir := demoDir()
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
	discoverRepos = func(context.Context, []string) ([]string, error) {
		cfg, err := config.Parse([]byte(demo.Config))
		if err != nil {
			return nil, err
		}
		var names []string
		for _, r := range cfg.Repos {
			names = append(names, r.FullName())
		}
		return names, nil
	}
}

// demoDir is deps-merger-demo in the user's own cache directory, which no
// other user can create first, as they could one in a shared temp dir. A
// fresh temp dir stands in when there is no cache directory, so the demo
// still runs, though it then forgets its verdicts between runs.
func demoDir() string {
	if cache, err := os.UserCacheDir(); err == nil {
		dir := filepath.Join(cache, "deps-merger-demo")
		if os.MkdirAll(dir, 0o700) == nil {
			return dir
		}
	}
	dir, err := os.MkdirTemp("", "deps-merger-demo-")
	if err != nil {
		panic("demo: no directory to keep its config in: " + err.Error())
	}
	return dir
}
