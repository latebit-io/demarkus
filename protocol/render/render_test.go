package render

import (
	"strings"
	"testing"
	"time"
)

func TestEscapeRoundTrip(t *testing.T) {
	tests := []string{"", "plain", `a|b`, `back\slash`, "[x](y) *b* _i_ `c` ~s~ #h", `\|`, "trailing\\"}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			if got := Unescape(Escape(in)); got != in {
				t.Errorf("Unescape(Escape(%q)) = %q", in, got)
			}
		})
	}
}

func TestEscapeCellFoldsLineBreaks(t *testing.T) {
	for _, in := range []string{"a\nb", "a\r\nb", "a\rb"} {
		if got := EscapeCell(in); got != "a b" {
			t.Errorf("EscapeCell(%q) = %q, want one space", in, got)
		}
	}
}

func TestListShapes(t *testing.T) {
	tests := []struct{ name, got, want string }{
		{"root heading", IndexHeading("/"), "\n# Index of /\n\n"},
		{"heading gains slash", IndexHeading("/docs"), "\n# Index of /docs/\n\n"},
		{"heading keeps slash", IndexHeading("/docs/"), "\n# Index of /docs/\n\n"},
		{"file", EntryLine("a b_c.md", false), "- [a b\\_c.md](a%20b_c.md)\n"},
		{"directory", EntryLine("sub", true), "- [sub/](sub/)\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestLookupShapes(t *testing.T) {
	row := &LookupRow{Path: "/a_b.md", Anchor: "sec-1", Importance: 0.5, Title: "T|itle\nmore", Tags: []string{"x", "y"}, Snippet: "hit"}
	tests := []struct{ name, got, want string }{
		{"catalog row drops snippet", LookupRowLine(row, false), "| /a\\_b.md#sec-1 | 0.50 | T\\|itle more | x, y |\n"},
		{"body row", LookupRowLine(row, true), "| /a\\_b.md#sec-1 | 0.50 | T\\|itle more | x, y | hit |\n"},
		{"heading", LookupHeading("q", "/s/"), "\n# Lookup matches for \"q\" in /s/\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
	if strings.Count(LookupHeader(true), "|")-strings.Count(LookupHeader(false), "|") != 2 {
		t.Error("body header must add exactly one column to both header lines")
	}
}

func TestVersionsShapes(t *testing.T) {
	when := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	if got, want := VersionLine("/a b.md", 3, when), "- [v3](%2Fa%20b.md/v3) - 2026-09-20T10:00:00Z\n"; got != want {
		t.Errorf("VersionLine = %q, want %q", got, want)
	}
	if got, want := VersionsHeading("/a_b.md"), "\n# Version History: /a\\_b.md\n\n"; got != want {
		t.Errorf("VersionsHeading = %q, want %q", got, want)
	}
}
