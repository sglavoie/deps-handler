//go:build !demo

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/review"
)

// The demo build replaces newAgent, so this only holds without it.
func TestNewAgent(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cfg := config.Agent{Use: "claude", Codex: config.Codex{Model: "gpt-5"}}
	if _, err := newAgent(cfg, ""); err == nil || err.Error() != "claude is not installed or not on PATH" {
		t.Fatalf("err = %v", err)
	}
	a, err := newAgent(cfg, "Codex")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := a.(review.Codex); !ok || c.Model != "gpt-5" {
		t.Fatalf("agent = %#v", a)
	}
}

func TestNewAPIAgent(t *testing.T) {
	cfg := config.API{URL: "http://127.0.0.1:8081/v1", Model: "qwen", KeyEnv: "DMT_TEST_KEY"}
	t.Setenv("DMT_TEST_KEY", "")
	if _, err := newAPIAgent(cfg); err == nil || !strings.Contains(err.Error(), "DMT_TEST_KEY, which is not set") {
		t.Fatalf("missing key: %v", err)
	}
	t.Setenv("DMT_TEST_KEY", "secret")
	a, err := newAPIAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if api := a.(review.API); api.Key != "secret" || !api.Tools || api.Model != "qwen" {
		t.Fatalf("agent = %+v", api)
	}
	if _, err := newAPIAgent(config.API{}); err == nil {
		t.Fatal("an agent without a URL was built")
	}
	// The api agent is not looked up on PATH: there is no program "api".
	if _, err := newAgent(config.Agent{Use: "api", API: config.API{URL: "http://x/v1", Model: "m"}}, ""); err != nil {
		t.Fatalf("newAgent: %v", err)
	}
}

// Off only in the defaults, reviews may be turned on by the repo's files,
// so the agent is set up, and here found missing. The demo build's agent
// is never missing.
func TestRunReviewSetsUpAgentOffOnlyInDefaults(t *testing.T) {
	_, _ = runFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no agent installed
	off := writeConfig(t, "defaults:\n  agent_review: false\nrepos:\n  - o/busy\n")
	if code, _, stderr := run("dependabot", "-c", off, "--review"); code == 0 || !strings.Contains(stderr, "claude is not installed") {
		t.Errorf("code %d, %q", code, stderr)
	}
}
