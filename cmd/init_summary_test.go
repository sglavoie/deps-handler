package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedib0t/go-pretty/v6/text"
)

func TestDiscoverySummaryLimitAndStdout(t *testing.T) {
	old, oldDiscover := ExampleConfig, discoverRepos
	t.Cleanup(func() { ExampleConfig, discoverRepos = old, oldDiscover })
	ExampleConfig = []byte("# Example config\nrepos:\n  # - acme/example\n")
	for _, n := range []int{1, 20, 23} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var repos []string
			for i := range n {
				repos = append(repos, fmt.Sprintf("owner/repository-%02d", i+1))
			}
			discoverRepos = func(context.Context, []string) ([]string, error) { return repos, nil }
			path := filepath.Join(t.TempDir(), "my config.yaml")
			code, out, stderr := run("init", "--discover", "-c", path)
			if code != 0 || stderr != "" || !strings.Contains(out, fmt.Sprintf("found %d repositories:", n)) {
				t.Fatalf("init = %d, %q, %q", code, out, stderr)
			}
			for _, repo := range repos[:min(20, n)] {
				if !strings.Contains(out, repo) {
					t.Errorf("missing %s", repo)
				}
			}
			if n > 20 && (!strings.Contains(out, "… and 3 more") || strings.Contains(out, repos[20])) {
				t.Fatal(out)
			}
			if n <= 20 && strings.Contains(out, "more") {
				t.Fatal(out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "  ") && text.StringWidth(line) > 78 {
					t.Errorf("unwrapped names: %q", line)
				}
			}
			if !strings.Contains(out, "`deps-handler dependabot -c '"+path+"'`") {
				t.Fatal(out)
			}
			written, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := run("init", "--discover", "--stdout")
			if code != 0 || stdout != string(written) || stderr != "" {
				t.Fatalf("stdout changed: %d, %q, %q", code, stdout, stderr)
			}
		})
	}
}
