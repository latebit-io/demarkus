package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/latebit/demarkus/client/internal/bookmarks"
	"github.com/latebit/demarkus/client/links"
)

// osc8 wraps text in an OSC 8 hyperlink sequence, matching Glamour v2 output.
func osc8(url, text string) string {
	return "\x1b]8;;" + url + "\x07" + text + "\x1b]8;;\x07"
}

func TestParseSchemeList(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{"empty disables", "", nil, false},
		{"default set", "http,https,gemini,mailto", []string{"http", "https", "gemini", "mailto"}, false},
		{"trims whitespace", "  http ,  https  ", []string{"http", "https"}, false},
		{"drops empty entries", "http,,https,", []string{"http", "https"}, false},
		{"preserves scheme special chars", "coap+tcp,svn-ssh,soap.beep", []string{"coap+tcp", "svn-ssh", "soap.beep"}, false},
		{"rejects space in entry", "http, foo bar, https", nil, true},
		{"rejects digit-leading scheme", "1http", nil, true},
		{"rejects special chars", "http@s", nil, true},
		{"rejects slash", "http/s", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSchemeList(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error for %q, got nil with result %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsExternalURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"mark://localhost/doc.md", false},
		{"mark://host:6309/path", false},
		{"MARK://host/doc.md", false}, // case-insensitive scheme matching
		{"Mark://host/doc.md", false},
		{"/index.md", false},
		{"./doc.md", false},
		{"doc.md", false},
		{"", false},
		{"http://example.com", true},
		{"https://example.com/path", true},
		{"gemini://example.org/", true},
		{"mailto:alice@example.com", true}, // opaque form, no //
		{"mailto://alice@example.com", true},
		{"file:///etc/passwd", true},
		{"javascript:alert(1)", true},
		{"1http://starts-with-digit", false}, // scheme must start with alpha
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := isExternalURL(tt.url); got != tt.want {
				t.Errorf("isExternalURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
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

// TestHandleBookmarkViewResetsLinkState regression-tests gh#81. Opening the
// bookmarks view must replace any stale link state carried over from the
// previously-viewed document, so Tab highlights a bookmark entry rather than
// re-rendering the prior page's marked content.
func TestHandleBookmarkViewResetsLinkState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bookmarks.md")
	content := "# Bookmarks\n\n- [A](mark://host/a.md) — 2026-01-01\n- [B](mark://host/b.md) — 2026-01-02\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := bookmarks.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := model{
		bookmarkStore:  store,
		links:          []string{"mark://host/stale.md"},
		linkInfos:      []links.LinkInfo{{Dest: "stale.md", Text: "stale"}},
		markedRendered: "stale-marked",
		linkRegions:    []linkRegion{{idx: 0}},
		linkIdx:        0,
		hoverIdx:       0,
	}
	out, _ := m.handleBookmarkView()
	got := out.(model)
	if got.status != "bookmarks" {
		t.Errorf("status = %q, want %q", got.status, "bookmarks")
	}
	if len(got.links) != 2 {
		t.Fatalf("links = %v, want 2 entries", got.links)
	}
	if got.links[0] != "mark://host/a.md" {
		t.Errorf("links[0] = %q, want mark://host/a.md", got.links[0])
	}
	if len(got.linkInfos) != 2 {
		t.Errorf("linkInfos len = %d, want 2", len(got.linkInfos))
	}
	if got.linkIdx != -1 {
		t.Errorf("linkIdx = %d, want -1", got.linkIdx)
	}
	if got.hoverIdx != -1 {
		t.Errorf("hoverIdx = %d, want -1", got.hoverIdx)
	}
	// ready is false, so no render path runs — stale markedRendered must still be cleared.
	if got.markedRendered != "" {
		t.Errorf("markedRendered = %q, want empty", got.markedRendered)
	}
	if got.linkRegions != nil {
		t.Errorf("linkRegions = %v, want nil", got.linkRegions)
	}
}

// TestHandleTabNavigationResumesFromScrollPosition verifies that after the
// user wheel-scrolls past the currently selected link, pressing Tab advances
// to the first link visible in the new scroll window instead of snapping
// back to wherever the cursor was before.
func TestHandleTabNavigationResumesFromScrollPosition(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(10))
	vp.SetContent(strings.Repeat("x\n", 60))
	vp.SetYOffset(30) // window now covers lines 30–39

	linkURLs := []string{"a", "b", "c", "d", "e"}
	regions := []linkRegion{
		{idx: 0, line: 1, startCol: 0, endCol: 1},
		{idx: 1, line: 5, startCol: 0, endCol: 1},
		{idx: 2, line: 32, startCol: 0, endCol: 1}, // visible
		{idx: 3, line: 34, startCol: 0, endCol: 1}, // visible
		{idx: 4, line: 50, startCol: 0, endCol: 1},
	}

	tests := []struct {
		name      string
		startIdx  int
		wantIdx   int
		wantFirst int // expected firstVisibleLinkIdx at current scroll
	}{
		{"selection above viewport jumps to first visible", 0, 2, 2},
		{"selection below viewport jumps to first visible", 4, 2, 2},
		{"no selection picks first visible", -1, 2, 2},
		{"selection already visible advances normally", 2, 3, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := model{
				viewport:       vp,
				links:          linkURLs,
				linkRegions:    regions,
				linkIdx:        tt.startIdx,
				hoverIdx:       -1,
				markedRendered: "sentinel", // non-empty so Tab enters the re-highlight branch
			}
			if got := m.firstVisibleLinkIdx(); got != tt.wantFirst {
				t.Errorf("firstVisibleLinkIdx() = %d, want %d", got, tt.wantFirst)
			}
			out, _ := m.handleTabNavigation()
			got := out.(model).linkIdx
			if got != tt.wantIdx {
				t.Errorf("linkIdx after Tab = %d, want %d", got, tt.wantIdx)
			}
		})
	}
}

// TestHandleWindowSizeRewrapsMarkdown verifies that a WindowSizeMsg triggers
// a markdown re-render at the new width. The rendered line count must change
// when the width shrinks enough to force rewrapping of a long paragraph.
func TestHandleWindowSizeRewrapsMarkdown(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(100), viewport.WithHeight(20))
	body := "# Title\n\n" + strings.Repeat("word ", 200) + "\n\n[link](a.md)\n"
	m := model{
		viewport:  vp,
		ready:     true,
		width:     100,
		height:    24,
		styleName: "dark",
		rawBody:   body,
		linkInfos: links.ExtractWithPositions(body),
		linkIdx:   -1,
		hoverIdx:  -1,
	}
	// Prime the cache at the initial width so the subsequent resize has something to re-render from.
	rendered, err := m.renderMarkdown(body)
	if err != nil {
		t.Fatalf("initial render: %v", err)
	}
	wideLines := strings.Count(rendered, "\n")

	out, _ := m.handleWindowSize(tea.WindowSizeMsg{Width: 40, Height: 24})
	got := out.(model)
	if got.markedRendered == "" {
		t.Fatal("markedRendered is empty after resize; expected re-render")
	}
	narrowLines := strings.Count(got.markedRendered, "\n")
	if narrowLines <= wideLines {
		t.Errorf("narrow-width render has %d lines, wide-width had %d — expected narrow > wide due to rewrap",
			narrowLines, wideLines)
	}
}

// TestHandleWindowSizeStaysInGraphModeWhileCrawling guards against a regression
// where resizing during the initial graph crawl (viewMode=viewGraph,
// graphNodes empty, crawling=true) fell through to the document rewrap branch
// and overwrote the "Crawling..." placeholder with the previous page body.
func TestHandleWindowSizeStaysInGraphModeWhileCrawling(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(100), viewport.WithHeight(20))
	m := model{
		viewport:  vp,
		ready:     true,
		width:     100,
		height:    24,
		styleName: "dark",
		viewMode:  viewGraph,
		crawling:  true,
		// Simulate a prior document still in rawBody — the bug was letting this leak through.
		rawBody:   "# Previous page\n\nThis should NOT be shown during crawl.\n",
		linkInfos: nil,
		linkIdx:   -1,
		hoverIdx:  -1,
	}
	out, _ := m.handleWindowSize(tea.WindowSizeMsg{Width: 40, Height: 24})
	got := out.(model)
	if got.markedRendered != "" {
		t.Errorf("markedRendered = %q, want empty (graph mode should not touch it)", got.markedRendered)
	}
	view := got.viewport.View()
	if !strings.Contains(view, "Crawling") {
		t.Errorf("viewport content missing Crawling placeholder after resize; got:\n%s", view)
	}
	if strings.Contains(view, "Previous page") {
		t.Errorf("viewport leaked previous document body during graph crawl; got:\n%s", view)
	}
}

// TestHandleWindowSizePersistsRewrapToHistory verifies that a resize updates
// the active history snapshot. Without this, restoreHistory (called by help
// dismiss, back, and forward) would restore the pre-resize wrap.
func TestHandleWindowSizePersistsRewrapToHistory(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(100), viewport.WithHeight(20))
	body := "# Title\n\n" + strings.Repeat("word ", 200) + "\n"
	m := model{
		viewport:  vp,
		ready:     true,
		width:     100,
		height:    24,
		styleName: "dark",
		rawBody:   body,
		linkInfos: links.ExtractWithPositions(body),
		linkIdx:   -1,
		hoverIdx:  -1,
		status:    "ok",
		history: []historyEntry{{
			url:            "mark://host/a.md",
			rendered:       "OLD-WIDTH-RENDER",
			markedRendered: "OLD-WIDTH-MARKED",
			rawBody:        body,
		}},
		histIdx: 0,
	}
	out, _ := m.handleWindowSize(tea.WindowSizeMsg{Width: 40, Height: 24})
	got := out.(model)
	if got.history[0].rendered == "OLD-WIDTH-RENDER" {
		t.Error("history[0].rendered was not refreshed on resize")
	}
	if got.history[0].markedRendered == "OLD-WIDTH-MARKED" {
		t.Error("history[0].markedRendered was not refreshed on resize")
	}
	if got.history[0].rendered == "" || got.history[0].markedRendered == "" {
		t.Errorf("history fields unexpectedly empty after resize: rendered=%q markedRendered=%q",
			got.history[0].rendered, got.history[0].markedRendered)
	}
}

// TestHandleWindowSizeRefreshesHistoryFromEntryBody verifies that a resize
// rewraps the history snapshot from the entry's own rawBody — not whatever
// the model's current rawBody happens to be. Without this, resizing while
// viewing bookmarks (or graph, or help) would either leave history at the
// pre-resize wrap (so esc/back snaps back) or, worse, write the bookmark
// body into the document's history slot. The right invariant: history is
// refreshed from entry.rawBody, independent of display mode.
func TestHandleWindowSizeRefreshesHistoryFromEntryBody(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(100), viewport.WithHeight(20))
	bookmarkBody := "# Bookmarks\n\n- [A](mark://host/a.md) — 2026-01-01\n"
	// Long paragraph in the entry so we can detect rewrapping by line count.
	docBody := "# Doc\n\n" + strings.Repeat("word ", 200) + "\n"
	m := model{
		viewport:  vp,
		ready:     true,
		width:     100,
		height:    24,
		styleName: "dark",
		rawBody:   bookmarkBody, // displayed content is bookmarks
		linkInfos: links.ExtractWithPositions(bookmarkBody),
		linkIdx:   -1,
		hoverIdx:  -1,
		status:    "bookmarks",
		history: []historyEntry{{
			url:            "mark://host/doc.md",
			rendered:       "STALE-RENDER",
			markedRendered: "STALE-MARKED",
			rawBody:        docBody,
			linkInfos:      links.ExtractWithPositions(docBody),
		}},
		histIdx: 0,
	}
	out, _ := m.handleWindowSize(tea.WindowSizeMsg{Width: 40, Height: 24})
	got := out.(model)

	// Stale entries must be replaced.
	if got.history[0].rendered == "STALE-RENDER" {
		t.Error("history[0].rendered was not refreshed on resize")
	}
	if got.history[0].markedRendered == "STALE-MARKED" {
		t.Error("history[0].markedRendered was not refreshed on resize")
	}
	// The refresh must come from entry.rawBody (docBody), not m.rawBody (bookmarkBody).
	if strings.Contains(got.history[0].rendered, "Bookmarks") ||
		strings.Contains(got.history[0].markedRendered, "Bookmarks") {
		t.Error("history refresh leaked bookmark content; should be sourced from entry.rawBody")
	}
	// Sanity: the refresh should contain markers of the doc body.
	if !strings.Contains(got.history[0].rendered, "Doc") {
		t.Errorf("history[0].rendered missing doc title; got:\n%s", got.history[0].rendered)
	}
}

// TestHandleWindowSizeRefreshesHistoryWhileHelpVisible simulates the exact
// scenario: open doc → press `?` → resize. The viewport keeps showing help
// (static), but history must still be rewrapped so dismissing help returns
// the document at the new width, not the pre-resize wrap.
func TestHandleWindowSizeRefreshesHistoryWhileHelpVisible(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(100), viewport.WithHeight(20))
	docBody := "# Doc\n\n" + strings.Repeat("word ", 200) + "\n"
	m := model{
		viewport:  vp,
		ready:     true,
		width:     100,
		height:    24,
		styleName: "dark",
		showHelp:  true,
		rawBody:   docBody,
		linkInfos: links.ExtractWithPositions(docBody),
		linkIdx:   -1,
		hoverIdx:  -1,
		status:    "ok",
		history: []historyEntry{{
			url:            "mark://host/doc.md",
			rendered:       "WIDE-WIDTH-RENDER",
			markedRendered: "WIDE-WIDTH-MARKED",
			rawBody:        docBody,
			linkInfos:      links.ExtractWithPositions(docBody),
		}},
		histIdx: 0,
	}
	out, _ := m.handleWindowSize(tea.WindowSizeMsg{Width: 40, Height: 24})
	got := out.(model)
	if got.history[0].rendered == "WIDE-WIDTH-RENDER" {
		t.Error("history not refreshed while help was showing; dismissing help would snap back to pre-resize wrap")
	}
	if got.history[0].markedRendered == "WIDE-WIDTH-MARKED" {
		t.Error("history markedRendered not refreshed while help was showing")
	}
}

// TestHandleMouseWheelClearsStaleHover verifies that a wheel scroll that
// actually moves the viewport clears m.hoverIdx. Hover state is built from
// the last MouseMotion's screen coords; if the content scrolls, those coords
// now point at a different link and the pre-scroll highlight is stale.
func TestHandleMouseWheelClearsStaleHover(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(5))
	// Content is taller than the viewport so wheel-down actually moves YOffset.
	vp.SetContent(strings.Repeat("line\n", 50))

	m := model{
		viewport:       vp,
		ready:          true,
		markedRendered: "some marked content",
		linkIdx:        -1,
		hoverIdx:       3,
	}
	out, _ := m.handleMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	got := out.(model)
	if got.hoverIdx != -1 {
		t.Errorf("hoverIdx = %d after wheel scroll, want -1", got.hoverIdx)
	}
}

// TestHandleMouseWheelDoesNotRehighlightOutsideDocumentView guards against
// processMarkers replaying a stale m.markedRendered over unrelated content.
// handleGraphToggle leaves markedRendered/hoverIdx intact when leaving the
// document, so a wheel scroll in graph/help mode must not run the rehighlight
// branch or the viewport gets overwritten with the previous document body.
func TestHandleMouseWheelDoesNotRehighlightOutsideDocumentView(t *testing.T) {
	tests := []struct {
		name     string
		viewMode viewMode
		showHelp bool
		status   string
	}{
		{"graph view", viewGraph, false, "ok"},
		{"help view", viewDocument, true, "ok"},
		{"bookmarks view", viewDocument, false, "bookmarks"},
	}
	const graphContent = "GRAPH-OR-HELP-CONTENT\n"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(5))
			vp.SetContent(graphContent + strings.Repeat("x\n", 50))
			m := model{
				viewport: vp,
				ready:    true,
				viewMode: tt.viewMode,
				showHelp: tt.showHelp,
				status:   tt.status,
				// Stale state left over from the previous document view.
				markedRendered: "PREVIOUS-DOCUMENT-MARKED",
				linkIdx:        -1,
				hoverIdx:       3,
			}
			out, _ := m.handleMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
			got := out.(model)
			if got.hoverIdx != 3 {
				t.Errorf("hoverIdx = %d, want 3 (preserved outside document view)", got.hoverIdx)
			}
			if strings.Contains(got.viewport.View(), "PREVIOUS-DOCUMENT-MARKED") {
				t.Errorf("wheel scroll leaked stale markedRendered into %s viewport", tt.name)
			}
		})
	}
}

// TestHandleMouseWheelPreservesHoverWhenNoScroll verifies that a wheel event
// that doesn't actually change YOffset (content fits in viewport) leaves
// hoverIdx alone — the link under the cursor hasn't moved.
func TestHandleMouseWheelPreservesHoverWhenNoScroll(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(50))
	// Content is shorter than the viewport — wheel-down is a no-op.
	vp.SetContent(strings.Repeat("line\n", 3))

	m := model{
		viewport:       vp,
		ready:          true,
		markedRendered: "some marked content",
		linkIdx:        -1,
		hoverIdx:       3,
	}
	out, _ := m.handleMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	got := out.(model)
	if got.hoverIdx != 3 {
		t.Errorf("hoverIdx = %d when no scroll occurred, want 3 (preserved)", got.hoverIdx)
	}
}
