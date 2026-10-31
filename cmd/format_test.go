package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/policy"
)

func TestReportOutputModes(t *testing.T) {
	for _, command := range []string{"dependabot", "repos", "verdicts", "diagnose", "fix", "rerun", "eval", "cases"} {
		t.Run(command, func(t *testing.T) {
			var args []string
			switch command {
			case "dependabot", "repos":
				_, cfg := runFixture(t)
				args = []string{command, "-c", cfg}
			case "verdicts":
				cache, _ := verdictsState(t)
				if err := cache.Put("o/r#7 lib@2", "abc123", "claude", policy.Verdict{Approved: true, Summary: "safe"}); err != nil {
					t.Fatal(err)
				}
				args = []string{"verdicts"}
			case "diagnose", "fix":
				mockPRCommands(t)
				args = []string{"pr", command, "o/r#7", "-c", writeConfig(t, "agent: {use: codex}\n")}
			case "rerun":
				mockRerunCommand(t)
				args = []string{"pr", "rerun", "o/r#7"}
			case "eval":
				mockEvalCommand(t, evalTaskAgent{answer: evalBillingAnswer})
				args = []string{"pr", "eval", "lyzma", "--agent", "claude"}
			case "cases":
				args = []string{"pr", "eval", "--list"}
			}
			t.Setenv("NO_COLOR", "1")
			_, defaultOutput, stderr := run(args...)
			if !strings.Contains(defaultOutput, "╭") || !strings.Contains(defaultOutput, "│") {
				t.Fatalf("default is not a table: %s\n%s", defaultOutput, stderr)
			}
			modes := [][]string{{"--table"}, {"--format", "table"}, {"--format", "list"}, {"--json"}, {"--format", "json"}}
			if command != "eval" && command != "cases" {
				modes = append(modes, []string{"--list"})
			}
			for _, flags := range modes {
				// fix can retain a checkout and return 1 when the fake repair cannot
				// complete; output selection must preserve that operational result.
				_, out, stderr := run(append(append([]string{}, args...), flags...)...)
				format := flags[len(flags)-1]
				switch format {
				case "--json", "json":
					if !json.Valid([]byte(out)) || strings.Contains(out, "\x1b") {
						t.Fatalf("%v: invalid JSON %s (%s)", flags, out, stderr)
					}
				case "--list", "list":
					if out == "" || strings.Contains(out, "╭") {
						t.Fatalf("%v: not the original output: %s (%s)", flags, out, stderr)
					}
				default:
					if !strings.Contains(out, "╭") {
						t.Fatalf("%v: missing table: %s (%s)", flags, out, stderr)
					}
				}
			}
		})
	}
}

func TestInvalidOutputModesFailBeforeSetup(t *testing.T) {
	commands := [][]string{{"dependabot"}, {"repos"}, {"verdicts"}, {"pr", "diagnose", "o/r#7"}, {"pr", "fix", "o/r#7"}, {"pr", "rerun", "o/r#7"}, {"pr", "eval"}}
	for _, args := range commands {
		invalid := [][]string{{"--format", "xml"}, {"--format", ""}, {"--table", "--json"}, {"--format", "list", "--json"}, {"--format", "json", "--table"}}
		if args[len(args)-1] != "eval" {
			invalid = append(invalid, []string{"--list", "--json"}, []string{"--list", "--table"})
		}
		for _, flags := range invalid {
			t.Run(strings.Join(append(append([]string{}, args...), flags...), " "), func(t *testing.T) {
				code, out, stderr := run(append(append([]string{}, args...), flags...)...)
				if code != 2 || out != "" || !(strings.Contains(stderr, "output modes conflict") || strings.Contains(stderr, "unknown output format")) {
					t.Fatalf("code %d, out %s, stderr %s", code, out, stderr)
				}
			})
		}
	}
	for _, flag := range []string{"--list", "--table", "--json", "--format=list"} {
		if code, _, stderr := run("dependabot", "--markdown", flag); code != 2 || !strings.Contains(stderr, "--markdown cannot") {
			t.Fatalf("%s: code %d: %s", flag, code, stderr)
		}
	}
}
