package cmd

import (
	"strings"
	"testing"
)

func TestSelectionSuggestions(t *testing.T) {
	cfg := writeConfig(t, "repos: [acme/storefront, acme/api, other/api, acme/worker]\n")
	for _, command := range []string{"dependabot", "repos"} {
		for _, tc := range []struct {
			flag, value, want string
		}{
			{"-r", "storefrnt", `repo "storefrnt" is not in the config; did you mean "storefront"?`},
			{"-r", " STOREFRNT ", `repo "STOREFRNT" is not in the config; did you mean "storefront"?`},
			{"-r", "acme/storefrnt", `repo "acme/storefrnt" is not in the config; did you mean "acme/storefront"?`},
			{"-r", "ap", `repo "ap" is not in the config; did you mean "acme/api"?`},
			{"--owner", "acm", `owner "acm" has no repo in the config; did you mean "acme"?`},
			{"--owner", "completely-unrelated", `owner "completely-unrelated" has no repo in the config`},
			{"-r", "completely-unrelated", `repo "completely-unrelated" is not in the config`},
			{"-r", "api", "ambiguous: api matches acme/api, other/api; use owner/name"},
		} {
			t.Run(command+tc.flag+tc.value, func(t *testing.T) {
				code, out, stderr := run(command, "-c", cfg, tc.flag, tc.value)
				if code != 2 || out != "" || stderr != "deps-handler: "+tc.want+"\n" {
					t.Fatalf("got %d, %q, %q; want usage error %q", code, out, stderr, tc.want)
				}
			})
		}
	}
}

func TestSelectionStillUsesExactMatches(t *testing.T) {
	cfg := writeConfig(t, "repos: [acme/storefront, acme/storefront2]\n")
	code, out, stderr := run("repos", "-c", cfg, "-r", " STOREFRONT ", "--json")
	if code != 0 || stderr != "" || !strings.Contains(out, "acme/storefront") || strings.Contains(out, "storefront2") {
		t.Fatalf("exact selection = %d, %q, %q", code, out, stderr)
	}
}

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"", "abc", 3}, {"abc", "", 3}, {"same", "same", 0},
		{"storefrnt", "storefront", 1}, {"acme", "amce", 2}, {"kitten", "sitting", 3},
		{"café", "cafe", 1},
	} {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
