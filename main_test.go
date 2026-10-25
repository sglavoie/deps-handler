package main

import (
	"bytes"
	"errors"
	"regexp"
	"slices"
	"testing"

	"github.com/sglavoie/deps-merger/internal/config"
)

// The example init writes lists no repos, so that no run reaches the
// example's, and is a config deps-merger accepts once they are filled in.
func TestExampleConfigParses(t *testing.T) {
	if _, err := config.Parse(exampleConfig); !errors.Is(err, config.ErrNoRepos) {
		t.Fatalf("example as written: err = %v, want ErrNoRepos", err)
	}
	// Uncomment the example repo entries, the lines under repos: (which
	// ends the file) that start an entry or continue one.
	head, entries, ok := bytes.Cut(exampleConfig, []byte("\nrepos:\n"))
	if !ok {
		t.Fatal("no repos: in the example")
	}
	entries = regexp.MustCompile(`(?m)^  # (-|  )`).ReplaceAll(entries, []byte("  $1"))
	cfg, err := config.Parse(slices.Concat(head, []byte("\nrepos:\n"), entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repos) != 4 || !cfg.Repos[2].Skip || cfg.Repos[3].Policy.RequireChecks {
		t.Fatalf("repos = %+v", cfg.Repos)
	}
}
