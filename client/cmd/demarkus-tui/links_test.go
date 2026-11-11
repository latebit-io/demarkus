package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/latebit-io/demarkus/client/internal/bookmarks"
	"github.com/latebit-io/demarkus/client/internal/termlinks"
	"github.com/latebit-io/demarkus/client/links"
)

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

// gh#81: opening the bookmarks view replaces the previous document's link
// state, so Tab highlights a bookmark, not the prior page's content.
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
		bookmarkStore: store,
		links:         []string{"mark://host/stale.md"},
		linkInfos:     []links.LinkInfo{{Dest: "stale.md", Text: "stale"}},
		rendered:      "stale-rendered",
		linkSegs:      []termlinks.Segment{{Index: 0, Start: 0, End: 5}},
		linkIdx:       0,
		hoverIdx:      0,
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
	// The list has no URL: it covers the history page, never replaces it.
	if got.currentURL != "" || got.pendingBody == "" {
		t.Errorf("currentURL = %q, pendingBody = %q; want the list shown over the history", got.currentURL, got.pendingBody)
	}

	// Rendered, the view owns its own link state: nothing of the stale page is left.
	m.ready, m.width, m.styleName = true, 80, "dark"
	m.viewport = viewport.New(viewport.WithWidth(80), viewport.WithHeight(10))
	out, _ = m.handleBookmarkView()
	got = out.(model)
	if got.rendered == "" || strings.Contains(got.rendered, "stale") || len(got.linkSegs) != 2 {
		t.Errorf("rendered = %q, segments = %+v; want the two bookmarks' own", got.rendered, got.linkSegs)
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
	regions := []termlinks.Region{
		{Index: 0, Line: 1, StartCol: 0, EndCol: 1},
		{Index: 1, Line: 5, StartCol: 0, EndCol: 1},
		{Index: 2, Line: 32, StartCol: 0, EndCol: 1}, // visible
		{Index: 3, Line: 34, StartCol: 0, EndCol: 1}, // visible
		{Index: 4, Line: 50, StartCol: 0, EndCol: 1},
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
			// Lines of plain text, each region's first cell lit on a repaint.
			segs := make([]termlinks.Segment, len(regions))
			rendered := strings.Repeat("x\n", 60)
			for i, r := range regions {
				segs[i] = termlinks.Segment{Region: r, Start: 2 * r.Line, End: 2*r.Line + 1}
			}
			m := model{
				viewport: vp,
				links:    linkURLs,
				linkSegs: segs,
				linkIdx:  tt.startIdx,
				hoverIdx: -1,
				rendered: rendered,
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
	if got.rendered == "" {
		t.Fatal("rendered is empty after resize; expected re-render")
	}
	narrowLines := strings.Count(got.rendered, "\n")
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
	if got.rendered != "" {
		t.Errorf("rendered = %q, want empty (graph mode should not touch it)", got.rendered)
	}
	view := got.viewport.View()
	if !strings.Contains(view, "Crawling") {
		t.Errorf("viewport content missing Crawling placeholder after resize; got:\n%s", view)
	}
	if strings.Contains(view, "Previous page") {
		t.Errorf("viewport leaked previous document body during graph crawl; got:\n%s", view)
	}
}

// History holds the body alone and renders it on restore, so a page comes
// back wrapped for the terminal as it is now: a resize while help, the graph
// or the bookmarks covered the page needs no refresh of the snapshot.
func TestRestoreHistoryRendersAtTheCurrentWidth(t *testing.T) {
	docBody := "# Doc\n\n" + strings.Repeat("word ", 200) + "\n\n[link](a.md)\n"
	shown := func(width int, showHelp bool) model {
		m := model{
			viewport:   viewport.New(viewport.WithWidth(100), viewport.WithHeight(20)),
			addressBar: textinput.New(),
			ready:      true,
			width:      100,
			height:     24,
			styleName:  "dark",
			showHelp:   showHelp,
			linkIdx:    -1,
			hoverIdx:   -1,
			history:    []historyEntry{{url: "mark://host/doc.md", rawBody: docBody, status: "ok"}},
		}
		out, _ := m.handleWindowSize(tea.WindowSizeMsg{Width: width, Height: 24})
		m = out.(model)
		m.restoreHistory()
		return m
	}
	wide, narrow := shown(100, true), shown(40, true)
	if wide.rendered == "" || strings.Count(narrow.rendered, "\n") <= strings.Count(wide.rendered, "\n") {
		t.Errorf("restored at width 40: %d lines, at 100: %d; want the narrow restore rewrapped",
			strings.Count(narrow.rendered, "\n"), strings.Count(wide.rendered, "\n"))
	}
	if narrow.currentURL != "mark://host/doc.md" || len(narrow.links) != 1 || len(narrow.linkSegs) == 0 {
		t.Errorf("restored links = %v, regions = %+v; want the document's link located", narrow.links, narrow.linkSegs)
	}
}

// Dropped history entries are cleared, not just sliced away: a reslice keeps
// their bodies reachable in the backing array.
func TestPushHistoryClearsWhatItDrops(t *testing.T) {
	var history []historyEntry
	idx := -1
	for i := range maxHistory + 5 {
		history, idx = pushHistory(history, idx, historyEntry{url: "u", rawBody: strings.Repeat("b", 8), status: strconv.Itoa(i)})
	}
	if len(history) != maxHistory || idx != maxHistory-1 || history[0].status != "5" {
		t.Fatalf("len = %d idx = %d oldest = %q; want the newest %d", len(history), idx, history[0].status, maxHistory)
	}
	// Going back three pages and loading a new one drops the forward entries.
	history, idx = pushHistory(history, idx-3, historyEntry{url: "new", rawBody: "n"})
	if len(history) != maxHistory-2 || history[idx].url != "new" {
		t.Fatalf("len = %d, newest = %q", len(history), history[idx].url)
	}
	for i, e := range history[:cap(history)][len(history):] {
		if e.rawBody != "" || e.url != "" {
			t.Fatalf("dropped entry %d still holds its page: %+v", i, e)
		}
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
		viewport: vp,
		ready:    true,
		rendered: "some rendered content",
		linkIdx:  -1,
		hoverIdx: 3,
	}
	out, _ := m.handleMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	got := out.(model)
	if got.hoverIdx != -1 {
		t.Errorf("hoverIdx = %d after wheel scroll, want -1", got.hoverIdx)
	}
}

// Leaving the document keeps rendered and hoverIdx, so a wheel scroll in
// graph or help mode must not rehighlight: that would paint the previous
// document over the view.
func TestHandleMouseWheelDoesNotRehighlightOutsideDocumentView(t *testing.T) {
	tests := []struct {
		name     string
		viewMode viewMode
		showHelp bool
		status   string
	}{
		{"graph view", viewGraph, false, "ok"},
		{"help view", viewDocument, true, "ok"},
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
				rendered: "PREVIOUS-DOCUMENT-RENDERED",
				linkIdx:  -1,
				hoverIdx: 3,
			}
			out, _ := m.handleMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
			got := out.(model)
			if got.hoverIdx != 3 {
				t.Errorf("hoverIdx = %d, want 3 (preserved outside document view)", got.hoverIdx)
			}
			if strings.Contains(got.viewport.View(), "PREVIOUS-DOCUMENT-RENDERED") {
				t.Errorf("wheel scroll leaked the stale document into %s viewport", tt.name)
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
		viewport: vp,
		ready:    true,
		rendered: "some rendered content",
		linkIdx:  -1,
		hoverIdx: 3,
	}
	out, _ := m.handleMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	got := out.(model)
	if got.hoverIdx != 3 {
		t.Errorf("hoverIdx = %d when no scroll occurred, want 3 (preserved)", got.hoverIdx)
	}
}

// A second click while a followed link is still loading must not index the
// cleared link list through regions left over from the previous page.
func TestClickDuringLoadDoesNotPanic(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(5))
	m := model{
		viewport:   vp,
		ready:      true,
		viewMode:   viewDocument,
		links:      []string{"mark://example.com/next.md"},
		linkSegs:   []termlinks.Segment{{Index: 0, Line: 0, StartCol: 0, EndCol: 10}},
		rendered:   "rendered",
		addressBar: textinput.New(),
	}
	click := tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 2}

	out, _ := m.handleMouseClick(click)
	loading := out.(model)
	if len(loading.linkSegs) != 0 || loading.rendered != "" {
		t.Fatalf("followLink kept stale regions: %d regions, rendered %q", len(loading.linkSegs), loading.rendered)
	}

	// Stale regions from any other path must still be ignored, not indexed.
	loading.linkSegs = []termlinks.Segment{{Index: 3, Line: 0, StartCol: 0, EndCol: 10}}
	if _, cmd := loading.handleMouseClick(click); cmd != nil {
		t.Fatal("click on an out-of-range region must not follow a link")
	}
}
