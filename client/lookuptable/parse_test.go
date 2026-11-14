package lookuptable

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/render"
)

// The input is what the server renders, so parser and producer cannot drift.
func TestParseTableReadsServerRendering(t *testing.T) {
	rows := []render.LookupRow{
		{Path: "/a_b.md", Anchor: "sec", Importance: 0.9, Title: "Pipes | and [brackets]", Tags: []string{"x", "y"}, Snippet: "a hit\nwrapped", Pins: []string{"/s.md/v2", "https://h/o/r/tree/abc1234/x_y"}},
		{Path: "/plain.md", Importance: 0, Title: "Plain", Tags: nil},
	}
	for _, c := range []render.Columns{{}, {Snippet: true}, {Pins: true}, {Snippet: true, Pins: true}} {
		var b strings.Builder
		b.WriteString(render.LookupHeading("q", "/"))
		b.WriteString(render.LookupHeader(c))
		for i := range rows {
			b.WriteString(render.LookupRowLine(&rows[i], c))
		}
		table, err := ParseTable(b.String())
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if table.Columns != c || len(table.Rows) != len(rows) {
			t.Fatalf("%+v: table = %+v", c, table)
		}
		got := table.Rows[0]
		if got.Path != "/a_b.md" || got.Anchor != "sec" || got.Importance != 0.9 || got.Title != "Pipes | and [brackets]" || got.Tags != "x, y" {
			t.Errorf("%+v: row = %+v", c, got)
		}
		if wantSnippet := map[bool]string{false: "", true: "a hit wrapped"}[c.Snippet]; got.Snippet != wantSnippet {
			t.Errorf("%+v: snippet = %q, want %q", c, got.Snippet, wantSnippet)
		}
		if wantPins := map[bool]string{false: "", true: "/s.md/v2, https://h/o/r/tree/abc1234/x_y"}[c.Pins]; got.Pins != wantPins {
			t.Errorf("%+v: pins = %q, want %q", c, got.Pins, wantPins)
		}
	}
}

func TestParseTableRejectsMalformed(t *testing.T) {
	header := render.LookupHeader(render.Columns{})
	tests := []struct {
		name string
		body string
	}{
		{name: "no table", body: "# Lookup\n\nnothing here\n"},
		{name: "wrong width", body: header + "| /a.md | 0.50 | T |\n"},
		{name: "importance out of range", body: header + "| /a.md | 1.50 | T | x |\n"},
		{name: "importance not a number", body: header + "| /a.md | high | T | x |\n"},
		{name: "relative path", body: header + "| a.md | 0.50 | T | x |\n"},
		{name: "pins row under catalog header", body: header + "| /a.md | 0.50 | T | x | /s.md/v1 |\n"},
		{name: "pins before snippet", body: "| Path | Importance | Title | Tags | Pins | Snippet |\n|---|---|---|---|---|---|\n| /a.md | 0.50 | T | x | p | s |\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseTable(tt.body); err == nil {
				t.Error("want an error")
			}
		})
	}
}
