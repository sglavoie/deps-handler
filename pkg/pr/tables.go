package pr

import (
	"crypto/rand"
	"io"
	"strconv"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/report"
	"github.com/sglavoie/deps-handler/pkg/review"
)

// reportTables substitutes trusted renderer output after cleaning all report
// prose. Random markers keep repository or agent text from naming a table.
// This preserves generated ANSI colors without trusting escapes in source data.
type reportTables struct {
	destination  io.Writer
	prefix       string
	replacements []string
}

func (t *reportTables) add(headers []string, rows [][]string) string {
	if t.prefix == "" {
		t.prefix = "DEPS_HANDLER_TABLE_" + rand.Text() + "_"
	}
	marker := t.prefix + strconv.Itoa(len(t.replacements)/2)
	t.replacements = append(t.replacements, marker, report.FormatTable(t.destination, headers, rows))
	return marker
}

func (t *reportTables) clean(value string) string {
	value = review.Clean(value)
	if len(t.replacements) > 0 {
		value = strings.NewReplacer(t.replacements...).Replace(value)
	}
	return value
}
