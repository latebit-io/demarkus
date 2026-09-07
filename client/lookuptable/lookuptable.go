// Package lookuptable is the grammar of a LOOKUP result table as the wire
// renders it: escaped cells, the header and separator rows, and the
// path#anchor location cell. Every surface that parses or rewrites the
// table uses this one copy.
package lookuptable

import "strings"

// Columns of the catalog table; body mode appends Snippet.
const (
	CatalogColumns = 4
	BodyColumns    = 5
)

var headerCells = []string{"Path", "Importance", "Title", "Tags", "Snippet"}

// Header renders the header and separator rows, body mode adding Snippet.
func Header(body bool) string {
	if body {
		return "| Path | Importance | Title | Tags | Snippet |\n|------|------------|-------|------|---------|\n"
	}
	return "| Path | Importance | Title | Tags |\n|------|------------|-------|------|\n"
}

// SplitRow splits a table line into its trimmed cells. ok is false when the
// line is not a pipe-delimited row. An escaped pipe stays inside its cell.
func SplitRow(line string) (cells []string, ok bool) {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
		return nil, false
	}
	cells = make([]string, 0, BodyColumns)
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
	return "| " + strings.Join(cells, " | ") + " |"
}

func validWidth(cells []string) bool {
	return len(cells) == CatalogColumns || len(cells) == BodyColumns
}

// IsHeader recognizes the catalog header and its body-mode extension.
func IsHeader(cells []string) bool {
	if !validWidth(cells) {
		return false
	}
	for i, want := range headerCells[:len(cells)] {
		if !strings.EqualFold(cells[i], want) {
			return false
		}
	}
	return true
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
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.ContainsRune(`\\[]()*_`+"`~#|", rune(s[i+1])) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var cellEscaper = strings.NewReplacer(
	`\`, `\\`,
	`[`, `\[`, `]`, `\]`,
	`(`, `\(`, `)`, `\)`,
	`*`, `\*`, `_`, `\_`,
	"`", "\\`", `~`, `\~`,
	`#`, `\#`, `|`, `\|`,
)

// Escape applies the server's markdown cell escaping; a line break of any
// form becomes one space.
func Escape(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	return cellEscaper.Replace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
}
