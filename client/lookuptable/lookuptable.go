// Package lookuptable is the grammar of a LOOKUP result table as the wire
// renders it: escaped cells, the header and separator rows, and the
// path#anchor location cell. Every surface that parses or rewrites the
// table uses this one copy.
package lookuptable

import (
	"strings"

	"github.com/latebit-io/demarkus/protocol/render"
)

// Columns of the catalog table; Snippet and Pins each append one.
const (
	catalogColumns = 4
	maxColumns     = 6
)

var headerCells = []string{"Path", "Importance", "Title", "Tags"}

// SplitRow splits a table line into its trimmed cells. ok is false when the
// line is not a pipe-delimited row. An escaped pipe stays inside its cell.
func SplitRow(line string) (cells []string, ok bool) {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
		return nil, false
	}
	cells = make([]string, 0, maxColumns)
	start := 1
	for i := 1; i < len(line)-1; i++ {
		if line[i] != '|' || charEscaped(line, i) {
			continue
		}
		cells = append(cells, strings.TrimSpace(line[start:i]))
		start = i + 1
	}
	cells = append(cells, strings.TrimSpace(line[start:len(line)-1]))
	return cells, true
}

// JoinRow renders cells as one table line in the wire's spacing.
func JoinRow(cells []string) string {
	return render.JoinRow(cells)
}

func validWidth(cells []string) bool {
	return len(cells) >= catalogColumns && len(cells) <= maxColumns
}

// headerColumns reads the optional columns a header row names: the catalog
// four, then Snippet, then Pins, each at most once and in that order.
func headerColumns(cells []string) (render.Columns, bool) {
	var c render.Columns
	if !validWidth(cells) {
		return c, false
	}
	for i, want := range headerCells {
		if !strings.EqualFold(cells[i], want) {
			return c, false
		}
	}
	rest := cells[catalogColumns:]
	if len(rest) > 0 && strings.EqualFold(rest[0], "Snippet") {
		c.Snippet, rest = true, rest[1:]
	}
	if len(rest) > 0 && strings.EqualFold(rest[0], "Pins") {
		c.Pins, rest = true, rest[1:]
	}
	return c, len(rest) == 0
}

// IsHeader recognizes the catalog header and its extensions.
func IsHeader(cells []string) bool {
	_, ok := headerColumns(cells)
	return ok
}

// HeaderOf reads the columns of body's result table from its header row
// alone, the rows unread; ok is false when body has no header.
func HeaderOf(body string) (render.Columns, bool) {
	_, columns, width := findHeader(strings.Split(body, "\n"))
	return columns, width > 0
}

// findHeader locates the first header row: its line, columns and width,
// width 0 when there is none.
func findHeader(lines []string) (line int, columns render.Columns, width int) {
	for i, text := range lines {
		cells, ok := SplitRow(text)
		if !ok {
			continue
		}
		if c, isHeader := headerColumns(cells); isHeader {
			return i, c, len(cells)
		}
	}
	return -1, render.Columns{}, 0
}

// IsSeparator recognizes the dashed row under the header.
func IsSeparator(cells []string) bool {
	if !validWidth(cells) {
		return false
	}
	for _, cell := range cells {
		trimmed := strings.Trim(cell, ":")
		if len(trimmed) < 3 || strings.Trim(trimmed, "-") != "" {
			return false
		}
	}
	return true
}

// IsDataRow reports a table row that carries a result: right width and
// neither header nor separator.
func IsDataRow(cells []string) bool {
	return validWidth(cells) && !IsHeader(cells) && !IsSeparator(cells)
}

// SplitLocation separates a row's escaped path cell from the anchor the
// server appended after escaping: the suffix past the first unescaped '#'.
// A '#' inside the path itself arrives escaped and stays in the path.
func SplitLocation(cell string) (path, anchor string) {
	for i := 0; i < len(cell); i++ {
		if cell[i] == '#' && !charEscaped(cell, i) {
			return Unescape(cell[:i]), cell[i+1:]
		}
	}
	return Unescape(cell), ""
}

func charEscaped(s string, at int) bool {
	backslashes := 0
	for i := at - 1; i >= 0 && s[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

// Unescape reverses the server's markdown cell escaping.
func Unescape(s string) string {
	return render.Unescape(s)
}

// Escape applies the server's markdown cell escaping; a line break of any
// form becomes one space.
func Escape(s string) string {
	return render.EscapeCell(s)
}
