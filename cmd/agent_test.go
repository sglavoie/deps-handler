//go:build !demo

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/review"
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
