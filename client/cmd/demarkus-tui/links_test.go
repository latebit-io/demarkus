package main

import (
	"strings"
	"testing"

	"github.com/latebit/demarkus/client/internal/links"
)

// osc8 wraps text in an OSC 8 hyperlink sequence, matching Glamour v2 output.
func osc8(url, text string) string {
	return "\x1b]8;;" + url + "\x07" + text + "\x1b]8;;\x07"
}

func TestInjectLinkMarkers(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		infos []links.LinkInfo
		check func(t *testing.T, result string)
	}{
		{
			name:  "no links",
			body:  "Just text.",
			infos: nil,
			check: func(t *testing.T, result string) {
				if result != "Just text." {
					t.Errorf("got %q, want %q", result, "Just text.")
				}
			},
		},
		{
			name: "single link gets markers via OSC 8",
			body: "see " + osc8("url.md", "hello") + " rest",
			infos: []links.LinkInfo{
				{Dest: "url.md", Text: "hello"},
			},
			check: func(t *testing.T, result string) {
				if !strings.Contains(result, string(markerStart(0))) {
					t.Error("missing start marker")
				}
				if !strings.Contains(result, string(markerEnd(0))) {
					t.Error("missing end marker")
				}
			},
		},
		{
			name: "multiple links get unique markers",
			body: osc8("a.md", "first") + " and " + osc8("b.md", "second") + " end",
			infos: []links.LinkInfo{
				{Dest: "a.md", Text: "first"},
				{Dest: "b.md", Text: "second"},
			},
			check: func(t *testing.T, result string) {
				if !strings.Contains(result, string(markerStart(0))) {
					t.Error("missing start marker for link 0")
				}
				if !strings.Contains(result, string(markerEnd(0))) {
					t.Error("missing end marker for link 0")
				}
				if !strings.Contains(result, string(markerStart(1))) {
					t.Error("missing start marker for link 1")
				}
				if !strings.Contains(result, string(markerEnd(1))) {
					t.Error("missing end marker for link 1")
				}
			},
		},
		{
			name: "matches link not preceding plain text with same word",
			body: "Hubs link to servers. " + osc8("hubs.md", "Hubs") + " list",
			infos: []links.LinkInfo{
				{Dest: "hubs.md", Text: "Hubs"},
			},
			check: func(t *testing.T, result string) {
				// Marker should be inside the OSC 8 region, not on the plain "Hubs".
				if !strings.Contains(result, string(markerStart(0))) {
					t.Error("missing start marker")
				}
				// The plain "Hubs" at position 0 should NOT have a marker before it.
				if strings.HasPrefix(result, string(markerStart(0))) {
					t.Error("marker incorrectly placed on plain text instead of hyperlink")
				}
			},
		},
		{
			name: "empty text skipped",
			body: "some text",
			infos: []links.LinkInfo{
				{Dest: "url.md", Text: "", OpenBracket: -1, CloseBracket: -1},
			},
			check: func(t *testing.T, result string) {
				if result != "some text" {
					t.Errorf("got %q, want %q", result, "some text")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := injectLinkMarkers(tt.body, tt.infos)
			tt.check(t, result)
		})
	}
}

func TestFindVisibleText(t *testing.T) {
	tests := []struct {
		name      string
		runes     string
		text      string
		from      int
		wantStart int
		wantEnd   int
	}{
		{
			name:      "plain text",
			runes:     "hello world",
			text:      "world",
			wantStart: 6,
			wantEnd:   11,
		},
		{
			name:      "with ANSI codes",
			runes:     "pre \x1b[35mhello\x1b[0m post",
			text:      "hello",
			wantStart: 9,
			wantEnd:   14,
		},
		{
			name:      "not found",
			runes:     "hello world",
			text:      "missing",
			wantStart: -1,
			wantEnd:   -1,
		},
		{
			name:      "from offset",
			runes:     "hello hello",
			text:      "hello",
			from:      5,
			wantStart: 6,
			wantEnd:   11,
		},
		{
			name:      "empty text",
			runes:     "hello",
			text:      "",
			wantStart: -1,
			wantEnd:   -1,
		},
		{
			name:      "skips OSC 8 hyperlink with BEL terminator",
			runes:     "pre \x1b]8;;http://example.com\x07hello\x1b]8;;\x07 post",
			text:      "hello",
			wantStart: 28,
			wantEnd:   33,
		},
		{
			name:      "skips OSC 8 hyperlink with ST terminator",
			runes:     "pre \x1b]8;;http://example.com\x1b\\hello\x1b]8;;\x1b\\ post",
			text:      "hello",
			wantStart: 29,
			wantEnd:   34,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := findVisibleText([]rune(tt.runes), []rune(tt.text), tt.from)
			if start != tt.wantStart {
				t.Errorf("start = %d, want %d", start, tt.wantStart)
			}
			if end != tt.wantEnd {
				t.Errorf("end = %d, want %d", end, tt.wantEnd)
			}
		})
	}
}

func TestProcessMarkers(t *testing.T) {
	s := func(i int) string { return string(markerStart(i)) }
	e := func(i int) string { return string(markerEnd(i)) }

	tests := []struct {
		name        string
		rendered    string
		selectedIdx int
		hoverIdx    int
		wantClean   string
		wantRegions int
	}{
		{
			name:        "no markers",
			rendered:    "plain text",
			selectedIdx: -1,
			hoverIdx:    -1,
			wantClean:   "plain text",
			wantRegions: 0,
		},
		{
			// Simulates glamour output: text + space + url + space + rest.
			// Region extends to cover the URL portion.
			name:        "single marker with url extends region",
			rendered:    "see " + s(0) + "hello" + e(0) + " /path.md rest",
			selectedIdx: -1,
			hoverIdx:    -1,
			wantClean:   "see hello /path.md rest",
			wantRegions: 1,
		},
		{
			name:        "selected marker gets reverse video including url",
			rendered:    "see " + s(0) + "hello" + e(0) + " /path.md rest",
			selectedIdx: 0,
			hoverIdx:    -1,
			wantClean:   "see \x1b[7mhello /path.md\x1b[27m rest",
			wantRegions: 1,
		},
		{
			name:        "hover highlights link",
			rendered:    "see " + s(0) + "hello" + e(0) + " /path.md rest",
			selectedIdx: -1,
			hoverIdx:    0,
			wantClean:   "see \x1b[7mhello /path.md\x1b[27m rest",
			wantRegions: 1,
		},
		{
			// Two links: "text0 url0 text1 url1"
			name:        "two links hover second",
			rendered:    s(0) + "a" + e(0) + " /a.md " + s(1) + "b" + e(1) + " /b.md end",
			selectedIdx: -1,
			hoverIdx:    1,
			wantClean:   "a /a.md \x1b[7mb /b.md\x1b[27m end",
			wantRegions: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, regions := processMarkers(tt.rendered, tt.selectedIdx, tt.hoverIdx)
			if cleaned != tt.wantClean {
				t.Errorf("cleaned = %q, want %q", cleaned, tt.wantClean)
			}
			if len(regions) != tt.wantRegions {
				t.Errorf("regions = %d, want %d", len(regions), tt.wantRegions)
			}
		})
	}
}

func TestProcessMarkersRegionPositions(t *testing.T) {
	s := func(i int) string { return string(markerStart(i)) }
	e := func(i int) string { return string(markerEnd(i)) }

	// "xx" + link text + space + url + space + more text.
	// Region should cover from "hello" through "/p.md" (columns 2-12).
	rendered := "xx" + s(0) + "hello" + e(0) + " /p.md end"
	_, regions := processMarkers(rendered, -1, -1)

	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	r := regions[0]
	if r.idx != 0 {
		t.Errorf("idx = %d, want 0", r.idx)
	}
	if r.line != 0 {
		t.Errorf("line = %d, want 0", r.line)
	}
	if r.startCol != 2 {
		t.Errorf("startCol = %d, want 2", r.startCol)
	}
	// "hello" (5) + " " (1) + "/p.md" (5) = 11 visual cols from start 2 = endCol 13
	if r.endCol != 13 {
		t.Errorf("endCol = %d, want 13", r.endCol)
	}
}

func TestProcessMarkersMultiLine(t *testing.T) {
	s := func(i int) string { return string(markerStart(i)) }
	e := func(i int) string { return string(markerEnd(i)) }

	rendered := "line0\n" + "pre " + s(0) + "link" + e(0) + " /url rest"
	_, regions := processMarkers(rendered, -1, -1)

	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	r := regions[0]
	if r.line != 1 {
		t.Errorf("line = %d, want 1", r.line)
	}
	if r.startCol != 4 {
		t.Errorf("startCol = %d, want 4", r.startCol)
	}
}

func TestProcessMarkersAnsiSkipped(t *testing.T) {
	s := func(i int) string { return string(markerStart(i)) }
	e := func(i int) string { return string(markerEnd(i)) }

	// ANSI codes should not affect visual column counting.
	rendered := "\x1b[1m" + s(0) + "hi" + e(0) + " /u.md\x1b[0m rest"
	_, regions := processMarkers(rendered, -1, -1)

	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	r := regions[0]
	if r.startCol != 0 {
		t.Errorf("startCol = %d, want 0", r.startCol)
	}
	// hi=2 + space=1 + /u.md=5 → endCol 8
	if r.endCol != 8 {
		t.Errorf("endCol = %d, want 8", r.endCol)
	}
}
