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
	cells := make([][]TableCell, len(rows))
	for i, row := range rows {
		for _, value := range row {
			cells[i] = append(cells[i], TableCell{Text: value})
		}
	}
	return formatTableCells(w, s, headers, cells)
}

// TableCell keeps presentation metadata separate from untrusted cell text.
// DimSuffix is appended after Text, with dim styling when colors are enabled.
type TableCell struct {
	Text, URL, DimSuffix string
}

// TableCells writes a bordered table with terminal links and styled suffixes.
// All supplied strings are sanitized before generated escapes are added.
func TableCells(w io.Writer, headers []string, rows [][]TableCell) error {
	_, err := io.WriteString(&errorWriter{w: w}, formatTableCells(w, styleFor(w), headers, rows)+"\n")
	return err
}

func formatTableCells(w io.Writer, s style, headers []string, rows [][]TableCell) string {
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
			c := TableCell{}
			if i < len(row) {
				c = row[i]
			}
			value, suffix := Plain(c.Text), Plain(c.DimSuffix)
			widths[i] = max(widths[i], text.StringWidth(value+suffix))
			r[i] = s.link(c.URL, s.paint(valueColor(value), value)+s.paint(dim, suffix))
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
	// go-pretty's hard wrapping understands SGR colors but can split an OSC 8
	// destination. Unwrap our generated link, wrap its label, then link each
	// line independently so neither the URL nor the table border is clickable.
	const linkEnd = "\x1b]8;;\x1b\\"
	if strings.HasPrefix(value, "\x1b]8;;") && strings.HasSuffix(value, linkEnd) {
		if start, label, ok := strings.Cut(value, "\x1b\\"); ok {
			lines := strings.Split(wrapTableCell(strings.TrimSuffix(label, linkEnd), width), "\n")
			for i, line := range lines {
				lines[i] = start + "\x1b\\" + line + linkEnd
			}
			return strings.Join(lines, "\n")
		}
	}
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
	// Only strip a glyph we generate for this exact status.
	if _, label, ok := strings.Cut(value, " "); ok && StatusLabel(label) == value {
		return labelColor(label)
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
