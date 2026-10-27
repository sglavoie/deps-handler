package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sglavoie/deps-merger/pkg/policy"
)

func TestCommandsReportOutputFailure(t *testing.T) {
	for _, name := range []string{"init", "init force", "repos", "empty verdicts", "forget verdict"} {
		t.Run(name, func(t *testing.T) {
			fixture := func(t *testing.T) []string {
				t.Helper()
				switch name {
				case "init", "init force":
					old := ExampleConfig
					ExampleConfig = []byte("repos: []\n")
					t.Cleanup(func() { ExampleConfig = old })
					path := filepath.Join(t.TempDir(), "config.yaml")
					if name == "init force" {
						path = writeConfig(t, "repos: [o/a]\n")
					}
					return []string{"init", "--force", "-c", path}
				case "repos":
					return []string{"repos", "-c", writeConfig(t, "repos: [o/a]\n")}
				default:
					cache, _ := verdictsState(t)
					if name == "empty verdicts" {
						return []string{"verdicts"}
					}
					if err := cache.Put("o/a#1 lib@2", "sha", "claude", policy.Verdict{Approved: true}); err != nil {
						t.Fatal(err)
					}
					return []string{"verdicts", "forget", "o/a"}
				}
			}
			code, output, stderr := run(fixture(t)...)
			if code != 0 {
				t.Fatalf("fixture failed: %s", stderr)
			}
			for _, after := range []int{0, len(output) / 2, len(output) - 1} {
				args := fixture(t)
				want := errors.New("output full")
				var stderr bytes.Buffer
				if code := execute(args, &failOutput{remaining: after, err: want}, &stderr); code != 1 || !bytes.Contains(stderr.Bytes(), []byte(want.Error())) {
					t.Errorf("after %d bytes: code %d, stderr %q; want output failure", after, code, stderr.String())
				}
			}
		})
	}
}
