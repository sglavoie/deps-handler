package config

import (
	"strings"
	"testing"
)

func TestParseAppliesDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  merge_method: merge
repos:
  - acme/Billing
  - repo: acme/auth-worker
    require_checks: false
    max_update_type: major
`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Repos[0].Policy, cfg.Repos[1].Policy
	if a != (Policy{MergeMethod: "merge", MaxUpdateType: UpdateMinor, RequireChecks: true}) {
		t.Errorf("Billing policy = %+v", a)
	}
	if b != (Policy{MergeMethod: "merge", MaxUpdateType: UpdateMajor, RequireChecks: false}) {
		t.Errorf("worker policy = %+v", b)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"no repos":      "repos: []",
		"bad name":      "repos: [acme]",
		"duplicate":     "repos: [a/b, A/B]",
		"bad method":    "repos: [{repo: a/b, merge_method: yolo}]",
		"bad update":    "repos: [{repo: a/b, max_update_type: huge}]",
		"unknown field": "repos: [{repo: a/b, auto: true}]",
		"unknown top":   "repo: [a/b]",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestFilter(t *testing.T) {
	cfg, err := Parse([]byte("repos: [acme/Billing, acme/Storefront]"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Filter([]string{"storefront"})
	if err != nil || len(got) != 1 || got[0].Name != "Storefront" {
		t.Fatalf("Filter(storefront) = %v, %v", got, err)
	}
	if _, err := cfg.Filter([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Filter(nope) err = %v", err)
	}
}
