package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/github"
)

func TestBareHelpWithStatusPointer(t *testing.T) {
	oldTerminal, oldClient, oldArgs := stdoutIsTerminal, newClient, os.Args
	t.Cleanup(func() { stdoutIsTerminal, newClient, os.Args = oldTerminal, oldClient, oldArgs })
	os.Args = []string{"dh"}
	newClient = func() (github.Client, error) {
		t.Error("bare invocation must never connect to GitHub")
		return nil, errors.New("unexpected GitHub connection")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no config", nil},
		{"usable config", []string{"-c", writeConfig(t, "repos:\n  - o/r\n")}},
		{"missing explicit config", []string{"-c", filepath.Join(t.TempDir(), "missing config.yaml")}},
		{"malformed config", []string{"-c", writeConfig(t, "repos: [")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdoutIsTerminal = func(io.Writer) bool { return false }
			code, plain, stderr := run(tc.args...)
			if code != 0 || stderr != "" || !strings.Contains(plain, "Usage:") || strings.Contains(plain, "For a read-only overview") {
				t.Fatalf("nonterminal = %d, %q, %q", code, plain, stderr)
			}
			stdoutIsTerminal = func(io.Writer) bool { return true }
			code, terminal, stderr := run(tc.args...)
			if code != 0 || stderr != "" || terminal != plain+"\nFor a read-only overview, run `"+displayCommand("status")+"`.\n" {
				t.Fatalf("terminal = %d, %q, %q", code, terminal, stderr)
			}
			for _, help := range []string{"--help", "help"} {
				args := append(append([]string{}, tc.args...), help)
				code, out, stderr := run(args...)
				if code != 0 || stderr != "" || out != plain {
					t.Errorf("explicit %s changed: %d, %q, %q", help, code, out, stderr)
				}
			}
		})
	}
	for _, args := range [][]string{{"--version"}, {"status", "--help"}, {"help", "status"}, {"unknown-command"}, {"--unknown-flag"}} {
		_, out, _ := run(args...)
		if strings.Contains(out, "For a read-only overview") {
			t.Errorf("unexpected pointer for %v: %s", args, out)
		}
	}
}

func TestDisplayCommandPreservesInvocationAndConfig(t *testing.T) {
	oldArgs, oldConfig := os.Args, configPath
	t.Cleanup(func() { os.Args, configPath = oldArgs, oldConfig })
	configPath = "my config.yaml"
	for _, name := range []string{"/bin/dh", "dh.exe", "deps-handler"} {
		os.Args = []string{name}
		want := "dh status -c 'my config.yaml'"
		if name == "deps-handler" {
			want = "deps-handler status -c 'my config.yaml'"
		}
		if got := displayCommand("status"); got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
}
