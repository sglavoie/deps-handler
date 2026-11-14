package report

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/sglavoie/deps-handler/pkg/github"
)

func TestPRListTableLinksAgesAndDrafts(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	prs := []github.ListedPR{
		{Repository: "o/r", Number: 1, Title: "New feature", Author: "dev", Draft: true, URL: "https://github.com/o/r/pull/1", CreatedAt: now.Add(-3 * 24 * time.Hour)},
		{Repository: "o/r", Number: 2, Title: "Fix", Author: "dev", URL: "https://github.com/o/r/pull/2", CreatedAt: now.Add(-35 * 24 * time.Hour)},
	}
	for _, s := range []style{{}, {links: true}, {color: true}, {links: true, color: true}} {
		t.Run(fmt.Sprint(s), func(t *testing.T) {
			out := formatPRListTable(io.Discard, s, prs, now)
			for _, want := range []string{"AGE", "3d", "5w", "o/r#1", "draft"} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q: %q", want, out)
				}
			}
			for _, unwanted := range []string{"CREATED", "DRAFT", "2026-", "yes", "no"} {
				if strings.Contains(out, unwanted) {
					t.Fatalf("unexpected %q: %q", unwanted, out)
				}
			}
			if strings.Contains(out, "URL") == s.links || strings.Contains(out, "\x1b]8;;") != s.links {
				t.Fatalf("link policy: %q", out)
			}
			if s.links && !strings.Contains(out, s.link(prs[0].URL, "o/r#1")) {
				t.Fatalf("PR cell link missing: %q", out)
			}
			if !s.links && !strings.Contains(out, prs[0].URL) {
				t.Fatalf("URL fallback missing: %q", out)
			}
			if strings.Contains(out, "\x1b[2m draft\x1b[0m") != s.color {
				t.Fatalf("draft styling: %q", out)
			}
		})
	}
	// Forced color on a pipe must not hide the URL column or enable OSC links.
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	var out bytes.Buffer
	if err := PRListTable(&out, prs, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "URL") || strings.Contains(out.String(), "\x1b]8;") {
		t.Fatal(out.String())
	}
}

func TestPRListAgeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{-time.Hour, "0m"}, {0, "0m"}, {59 * time.Second, "0m"},
		{time.Minute, "1m"}, {time.Hour, "1h"}, {24 * time.Hour, "1d"},
		{6 * 24 * time.Hour, "6d"}, {7 * 24 * time.Hour, "1w"}, {35 * 24 * time.Hour, "5w"},
	} {
		if got := shortAge(tc.age); got != tc.want {
			t.Errorf("age %s = %q, want %q", tc.age, got, tc.want)
		}
	}
	now := time.Now()
	for _, tc := range []struct {
		at   time.Time
		want string
	}{{time.Time{}, "-"}, {now.Add(-5 * 24 * time.Hour), "5d ago"}, {now.Add(9 * 24 * time.Hour), "in 9d"}} {
		if got := RelativeTime(tc.at, now); got != tc.want {
			t.Errorf("relative time = %q, want %q", got, tc.want)
		}
	}
}

func TestStructuredTableLinksWrapAndSanitize(t *testing.T) {
	s := style{links: true, color: true}
	url := "https://github.com/o/" + strings.Repeat("repository", 20) + "/pull/1\x07\x1b\u202e"
	value := strings.Repeat("界", 100) + "\x1b[2J\x07\u202e"
	out := formatTableCells(io.Discard, s, []string{"PR", "TITLE"}, [][]TableCell{{
		{Text: value, URL: url}, {Text: strings.Repeat("title ", 50), DimSuffix: " draft\x1b[2J\x07"},
	}})
	if strings.ContainsAny(out, "\x07\u202e") || strings.Contains(out, "\x1b[2J") || !strings.Contains(out, "\x1b]8;;"+Plain(url)+"\x1b\\") {
		t.Fatalf("unsafe or missing link: %q", out)
	}
	width := -1
	for _, line := range strings.Split(out, "\n") {
		n := text.StringWidthWithoutEscSequences(line)
		if width == -1 {
			width = n
		}
		if n != width || n > 140 {
			t.Fatalf("uneven wrapped width %d, want %d: %q", n, width, out)
		}
	}
	if strings.Count(out, "界") != 100 || strings.Count(out, "title") != 50 {
		t.Fatalf("lost wrapped content: %q", out)
	}
}
