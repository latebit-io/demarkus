package lookuptable

import (
	"slices"
	"testing"
)

func TestSplitRowKeepsEscapedPipes(t *testing.T) {
	cells, ok := SplitRow(`| /a\|b.md | 0.90 | A \| B | x, y |`)
	if !ok {
		t.Fatal("row not recognized")
	}
	want := []string{`/a\|b.md`, "0.90", `A \| B`, "x, y"}
	if !slices.Equal(cells, want) {
		t.Fatalf("cells = %q, want %q", cells, want)
	}
	if _, ok := SplitRow("# Lookup matches"); ok {
		t.Fatal("heading parsed as a row")
	}
	if got := JoinRow(cells); got != `| /a\|b.md | 0.90 | A \| B | x, y |` {
		t.Fatalf("JoinRow = %q", got)
	}
}

func TestHeaderSeparatorDataRow(t *testing.T) {
	header, _ := SplitRow("| Path | Importance | Title | Tags |")
	bodyHeader, _ := SplitRow("| Path | Importance | Title | Tags | Snippet |")
	sep, _ := SplitRow("|------|------------|-------|------|")
	data, _ := SplitRow("| /a.md | 0.5 | A | a |")
	short, _ := SplitRow("| a | b |")
	if !IsHeader(header) || !IsHeader(bodyHeader) || IsHeader(data) {
		t.Fatal("IsHeader wrong")
	}
	if !IsSeparator(sep) || IsSeparator(data) {
		t.Fatal("IsSeparator wrong")
	}
	if !IsDataRow(data) || IsDataRow(header) || IsDataRow(sep) || IsDataRow(short) {
		t.Fatal("IsDataRow wrong")
	}
}

func TestSplitLocation(t *testing.T) {
	for _, tt := range []struct{ cell, path, anchor string }{
		{`/docs/a.md#intro`, "/docs/a.md", "intro"},
		{`/docs/a\#b.md#intro`, "/docs/a#b.md", "intro"},
		{`/docs/a\_b.md`, "/docs/a_b.md", ""},
	} {
		if path, anchor := SplitLocation(tt.cell); path != tt.path || anchor != tt.anchor {
			t.Errorf("SplitLocation(%q) = %q, %q; want %q, %q", tt.cell, path, anchor, tt.path, tt.anchor)
		}
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	in := "a|b [c] (d) *e* _f_ `g` ~h~ #i \\j\nk\r\nl"
	if got := Unescape(Escape(in)); got != "a|b [c] (d) *e* _f_ `g` ~h~ #i \\j k l" {
		t.Fatalf("round trip = %q", got)
	}
}
