package report

import (
	"fmt"
	"io"
	"os"
	"strings"

	pretty "github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/sglavoie/deps-handler/pkg/runner"
	"golang.org/x/term"
)

// Table writes a bordered table. Values are sanitized before styling; long
// cells wrap without losing content, including wide Unicode characters.
func Table(w io.Writer, headers []string, rows [][]string) error {
	_, err := io.WriteString(&errorWriter{w: w}, FormatTable(w, headers, rows)+"\n")
	return err
}

// FormatTable builds a table using the destination's terminal/color settings.
// This also lets a larger report be assembled before its first output write.
func FormatTable(w io.Writer, headers []string, rows [][]string) string {
	return formatTable(w, styleFor(w), headers, rows)
}

func formatTable(w io.Writer, s style, headers []string, rows [][]string) string {
	if len(headers) == 0 {
		return ""
	}
	t := pretty.NewWriter()
	t.SetStyle(pretty.StyleRounded)
	t.Style().Format.Header = text.FormatDefault
	header := make(pretty.Row, len(headers))
	widths := make([]int, len(headers))
	for i, h := range headers {
		header[i] = s.paint(bold+";"+cyan, h)
		widths[i] = text.StringWidth(Plain(h))
	}
	t.AppendHeader(header)
	for _, row := range rows {
		r := make(pretty.Row, len(headers))
		for i := range headers {
			value := ""
			if i < len(row) {
				value = Plain(row[i])
			}
			widths[i] = max(widths[i], text.StringWidth(value))
			r[i] = s.paint(valueColor(value), value)
		}
		t.AppendRow(r)
	}
	// Shrink the widest columns first. Wrapping preserves reasons and commands
	// rather than cutting them off at the viewport edge.
	width := 140
	destination := w
	if ew, ok := w.(*errorWriter); ok {
		destination = ew.w
	}
	if f, ok := destination.(*os.File); ok {
		if columns, _, err := term.GetSize(int(f.Fd())); err == nil {
			width = columns
		}
	}
	budget := max(len(headers)*4, width-3*len(headers)-1)
	for i := range widths {
		widths[i] = min(widths[i], budget)
	}
	for {
		total, widest := 0, 0
		for i, v := range widths {
			total += v
			if v > widths[widest] {
				widest = i
			}
		}
		if total <= budget || widths[widest] <= 4 {
			break
		}
		widths[widest]--
	}
	columns := make([]pretty.ColumnConfig, len(headers))
	for i, width := range widths {
		columns[i] = pretty.ColumnConfig{Number: i + 1, WidthMax: max(4, width), WidthMaxEnforcer: wrapTableCell, Align: text.AlignLeft}
	}
	t.SetColumnConfigs(columns)
	return t.Render()
}

// go-pretty can place a double-width rune in the last single-width slot.
// Rewrapping one cell narrower leaves room for that rune and keeps borders
// aligned without truncating its contents or stripping generated ANSI colors.
func wrapTableCell(value string, width int) string {
	wrapped := text.WrapSoft(value, width)
	for _, line := range strings.Split(wrapped, "\n") {
		if text.StringWidthWithoutEscSequences(line) > width {
			return text.WrapSoft(value, max(1, width-1))
		}
	}
	return wrapped
}

func valueColor(value string) string {
	switch strings.ToLower(value) {
	case "approved", "pass", "passed", "passing", "yes", "patch":
		return green
	case "rejected", "fail", "failed", "failure", "major":
		return red
	case "pending", "waiting", "minor", "unknown", "stale":
		return yellow
	case "no", "any", "skipped":
		return dim
	}
	return labelColor(value)
}

// RepoTable writes the table view of a repository, retaining the same policy,
// warnings and findings as Repo, and the same event-versus-decision rules.
func RepoTable(w io.Writer, apply bool, res runner.RepoResult) error {
	out := &errorWriter{w: w}
	s := styleFor(w)
	fmt.Fprintln(out, s.paint(bold, res.Repo)+"  "+s.paint(dim, "("+header(res)+")"))
	repoBodyTable(out, apply, res, s)
	return out.err
}
