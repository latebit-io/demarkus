package lookuptable

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/render"
)

// The input is what the server renders, so parser and producer cannot drift.
func TestParseTableReadsServerRendering(t *testing.T) {
	rows := []render.LookupRow{
		{Path: "/a_b.md", Anchor: "sec", Importance: 0.9, Title: "Pipes | and [brackets]", Tags: []string{"x", "y"}, Snippet: "a hit\nwrapped"},
		{Path: "/plain.md", Importance: 0, Title: "Plain", Tags: nil},
	}
	for _, body := range []bool{false, true} {
		var b strings.Builder
		b.WriteString(render.LookupHeading("q", "/"))
		b.WriteString(render.LookupHeader(body))
		for i := range rows {
			b.WriteString(render.LookupRowLine(&rows[i], body))
		}
		table, err := ParseTable(b.String())
		if err != nil {
			t.Fatalf("body=%v: %v", body, err)
		}
		if table.Body != body || len(table.Rows) != len(rows) {
			t.Fatalf("body=%v: table = %+v", body, table)
		}
		got := table.Rows[0]
		if got.Path != "/a_b.md" || got.Anchor != "sec" || got.Importance != 0.9 || got.Title != "Pipes | and [brackets]" || got.Tags != "x, y" {
			t.Errorf("body=%v: row = %+v", body, got)
		}
		if wantSnippet := map[bool]string{false: "", true: "a hit wrapped"}[body]; got.Snippet != wantSnippet {
			t.Errorf("body=%v: snippet = %q, want %q", body, got.Snippet, wantSnippet)
		}
	}
}

func TestParseTableRejectsMalformed(t *testing.T) {
	header := render.LookupHeader(false)
	tests := []struct {
		name string
		body string
	}{
		{name: "no table", body: "# Lookup\n\nnothing here\n"},
		{name: "wrong width", body: header + "| /a.md | 0.50 | T |\n"},
		{name: "importance out of range", body: header + "| /a.md | 1.50 | T | x |\n"},
		{name: "importance not a number", body: header + "| /a.md | high | T | x |\n"},
		{name: "relative path", body: header + "| a.md | 0.50 | T | x |\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseTable(tt.body); err == nil {
				t.Error("want an error")
			}
		})
	}
}
