package report

import (
	"fmt"
	"io"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
)

// PRListTable writes PRs with terminal links, compact ages and draft markers.
func PRListTable(w io.Writer, prs []github.ListedPR, now time.Time) error {
	_, err := io.WriteString(&errorWriter{w: w}, formatPRListTable(w, styleFor(w), prs, now)+"\n")
	return err
}

func formatPRListTable(w io.Writer, s style, prs []github.ListedPR, now time.Time) string {
	headers := []string{"PR", "TITLE", "AUTHOR", "AGE"}
	if !s.links {
		headers = append(headers, "URL")
	}
	rows := make([][]TableCell, 0, len(prs))
	for _, p := range prs {
		title := TableCell{Text: p.Title}
		if p.Draft {
			title.DimSuffix = " draft"
		}
		age := "-"
		if !p.CreatedAt.IsZero() {
			age = shortAge(now.Sub(p.CreatedAt))
		}
		row := []TableCell{
			{Text: fmt.Sprintf("%s#%d", p.Repository, p.Number), URL: p.URL},
			title, {Text: p.Author}, {Text: age},
		}
		if !s.links {
			row = append(row, TableCell{Text: p.URL})
		}
		rows = append(rows, row)
	}
	return formatTableCells(w, s, headers, rows)
}

func shortAge(d time.Duration) string {
	d = max(0, d)
	for _, unit := range []struct {
		duration time.Duration
		suffix   string
	}{{7 * 24 * time.Hour, "w"}, {24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if d >= unit.duration {
			return fmt.Sprintf("%d%s", d/unit.duration, unit.suffix)
		}
	}
	return "0m"
}

// RelativeTime describes a past or future timestamp using whole time units.
func RelativeTime(at, now time.Time) string {
	if at.IsZero() {
		return "-"
	}
	duration := now.Sub(at)
	if at.After(now) {
		duration = at.Sub(now)
	}
	age := shortAge(duration)
	if duration >= 24*time.Hour {
		age = fmt.Sprintf("%dd", duration/(24*time.Hour))
	}
	if at.After(now) {
		return "in " + age
	}
	return age + " ago"
}
