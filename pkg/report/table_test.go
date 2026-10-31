package report

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

func TestTableWrapsUnicodeAndSanitizesCells(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	value := strings.Repeat("界", 100) + " e\u0301 " + strings.Repeat("word ", 80) + "tail"
	output := FormatTable(io.Discard, []string{"NAME", "DETAIL"}, [][]string{{"项目", value + "\x1b[2J\x07\u202e"}})
	if strings.ContainsAny(output, "\x1b\x07\u202e") || !strings.Contains(output, "项目") || !strings.Contains(output, "tail") {
		t.Fatalf("unsafe or incomplete table: %q", output)
	}
	width := -1
	for _, line := range strings.Split(output, "\n") {
		current := text.StringWidth(line)
		if width == -1 {
			width = current
		}
		if current != width || current > 140 {
			t.Fatalf("uneven table width %d (want %d, at most 140): %q", current, width, line)
		}
	}
	if strings.Count(output, "界") != 100 || strings.Count(output, "word") != 80 {
		t.Fatal("wrapping lost content", output)
	}
}

func TestTableColorSettings(t *testing.T) {
	for _, tc := range []struct {
		name, noColor, cliColor, force string
		colored                        bool
	}{
		{name: "pipe"},
		{name: "forced", force: "1", colored: true},
		{name: "no color wins", noColor: "1", force: "1"},
		{name: "clicolor disabled", cliColor: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			t.Setenv("CLICOLOR", tc.cliColor)
			t.Setenv("CLICOLOR_FORCE", tc.force)
			var out bytes.Buffer
			if err := RepoTable(&out, true, applied); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "\x1b[") != tc.colored {
				t.Fatalf("wrong colors: %q", out.String())
			}
			if tc.colored && (!strings.Contains(out.String(), "\x1b[32mmerged") || !strings.Contains(out.String(), "\x1b[1;31mmerge-failed")) {
				t.Fatalf("missing status colors: %q", out.String())
			}
		})
	}
}

func TestRepoTablePreservesEventsAfterFailure(t *testing.T) {
	result := applied
	result.Error = "read failed"
	result.Warnings = []string{"cache warning"}
	var out bytes.Buffer
	if err := RepoTable(&out, true, result); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"read failed", "merged", "merge-failed", "cache warning"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out.String())
		}
	}
	for _, stale := range []string{"#3", "no checks ran", "needs attention"} {
		if strings.Contains(out.String(), stale) {
			t.Errorf("stale decision or finding %q: %s", stale, out.String())
		}
	}
}

type shortTableWriter struct{}

func (shortTableWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestTablesReportWriteFailures(t *testing.T) {
	for _, render := range []func(io.Writer) error{
		func(w io.Writer) error { return Table(w, []string{"NAME"}, [][]string{{"value"}}) },
		func(w io.Writer) error { return RepoTable(w, true, runner.RepoResult{Repo: "o/r"}) },
	} {
		if err := render(shortTableWriter{}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write: %v", err)
		}
	}
}
