// Package render writes the markdown bodies of LIST, LOOKUP and VERSIONS
// responses. The server renders with it and clients and test fakes reuse it,
// so a parser is always tested against what the wire really carries.
package render

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var escaper = strings.NewReplacer(
	`\`, `\\`,
	`[`, `\[`, `]`, `\]`,
	`(`, `\(`, `)`, `\)`,
	`*`, `\*`, `_`, `\_`,
	"`", "\\`", `~`, `\~`,
	`#`, `\#`, `|`, `\|`,
)

var lineBreaks = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

// escapable is every byte Escape puts a backslash before.
const escapable = `\[]()*_` + "`~#|"

// Escape makes s literal markdown text.
func Escape(s string) string {
	return escaper.Replace(s)
}

// EscapeCell is Escape for a table cell: a line break of any form becomes
// one space, since a raw one would end the row.
func EscapeCell(s string) string {
	return escaper.Replace(lineBreaks.Replace(s))
}

// Unescape reverses Escape.
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(escapable, s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// EscapePath escapes one path or path segment for a link destination.
func EscapePath(s string) string {
	return url.PathEscape(s)
}

// IndexHeading opens a LIST page; a directory path always shows a trailing slash.
func IndexHeading(dirPath string) string {
	if dirPath != "/" && !strings.HasSuffix(dirPath, "/") {
		dirPath += "/"
	}
	return "\n# Index of " + Escape(dirPath) + "\n\n"
}

// EntryLine is one LIST entry; a directory carries a trailing slash.
func EntryLine(name string, isDir bool) string {
	display, link := Escape(name), EscapePath(name)
	if isDir {
		return "- [" + display + "/](" + link + "/)\n"
	}
	return "- [" + display + "](" + link + ")\n"
}

// LookupRow is one LOOKUP match. Anchor and Snippet are body mode only.
type LookupRow struct {
	Path       string
	Anchor     string
	Importance float64
	Title      string
	Tags       []string
	Snippet    string
}

// LookupHeading opens a LOOKUP result.
func LookupHeading(query, scope string) string {
	return "\n# Lookup matches for \"" + Escape(query) + "\" in " + Escape(scope) + "\n\n"
}

// LookupHeader is the table header and separator; body mode adds Snippet.
func LookupHeader(body bool) string {
	if body {
		return "| Path | Importance | Title | Tags | Snippet |\n|------|------------|-------|------|---------|\n"
	}
	return "| Path | Importance | Title | Tags |\n|------|------------|-------|------|\n"
}

// JoinRow renders already escaped cells as one table line, no newline.
func JoinRow(cells []string) string {
	return "| " + strings.Join(cells, " | ") + " |"
}

// LookupRowLine renders one match. The anchor follows the escaped path
// unescaped: a slug cannot break the table, and clients fetch path#anchor next.
func LookupRowLine(row *LookupRow, body bool) string {
	location := EscapeCell(row.Path)
	if row.Anchor != "" {
		location += "#" + row.Anchor
	}
	cells := []string{
		location,
		strconv.FormatFloat(row.Importance, 'f', 2, 64),
		EscapeCell(row.Title),
		EscapeCell(strings.Join(row.Tags, ", ")),
	}
	if body {
		cells = append(cells, EscapeCell(row.Snippet))
	}
	return JoinRow(cells) + "\n"
}

// VersionsHeading opens a VERSIONS body.
func VersionsHeading(docPath string) string {
	return "\n# Version History: " + Escape(docPath) + "\n\n"
}

// VersionLine is one VERSIONS entry, newest first in a real response.
func VersionLine(docPath string, version int, modified time.Time) string {
	return fmt.Sprintf("- [v%d](%s/v%d) - %s\n", version, EscapePath(docPath), version, modified.Format(time.RFC3339))
}
