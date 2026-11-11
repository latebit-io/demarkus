package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/memory/graph"
	"github.com/latebit-io/demarkus/protocol"
)

func fetched(t *testing.T, m model, url, body string) model {
	t.Helper()
	out, _ := m.handleFetchResult(fetchResult{
		url: url, seq: m.fetchSeq,
		result: fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: body}},
	})
	return out.(model)
}

func pressed(t *testing.T, m model, key tea.KeyPressMsg) model {
	t.Helper()
	out, _ := m.handleViewportKey(key)
	return out.(model)
}

// A failed fetch pushes no history, so back from its error is the page the
// error covers, not the one before it; Esc leaves the error the same way.
func TestBackFromAFailedFetchReturnsToThePageItCovered(t *testing.T) {
	m := shownDocument(t, "mark://host/a.md")
	m = fetched(t, m, "mark://host/b.md", "# B\n")
	for name, key := range map[string]tea.KeyPressMsg{"back": {Code: '['}, "esc": {Code: tea.KeyEscape}} {
		out, _ := m.handleFetchResult(fetchResult{url: "mark://host/missing.md", seq: m.fetchSeq, err: errBoom})
		failed := out.(model)
		if !failed.offHistory || failed.err == nil {
			t.Fatalf("%s: failed fetch = offHistory %v err %v", name, failed.offHistory, failed.err)
		}
		back := pressed(t, failed, key)
		if back.currentURL != "mark://host/b.md" || back.err != nil || back.histIdx != 1 || back.offHistory {
			t.Errorf("%s from the error: url %q err %v histIdx %d; want page B again", name, back.currentURL, back.err, back.histIdx)
		}
	}
	// Once B is on screen again, back is the page before it.
	out, _ := m.handleFetchResult(fetchResult{url: "mark://host/missing.md", seq: m.fetchSeq, err: errBoom})
	if a := pressed(t, pressed(t, out.(model), tea.KeyPressMsg{Code: '['}), tea.KeyPressMsg{Code: '['}); a.currentURL != "mark://host/a.md" {
		t.Errorf("second back = %q, want page A", a.currentURL)
	}
}

// With a single page, a failed fetch used to leave no way back to it.
func TestBackFromAFailedFetchWorksWithOnePage(t *testing.T) {
	m := shownDocument(t, "mark://host/only.md")
	out, _ := m.handleFetchResult(fetchResult{url: "mark://host/missing.md", seq: m.fetchSeq, err: errBoom})
	if back := pressed(t, out.(model), tea.KeyPressMsg{Code: '['}); back.currentURL != "mark://host/only.md" || back.err != nil {
		t.Errorf("back = %q err %v, want the only page", back.currentURL, back.err)
	}
}

func TestBackFromBookmarksReturnsToThePageTheyCovered(t *testing.T) {
	m := shownDocument(t, "mark://host/a.md")
	m = fetched(t, m, "mark://host/b.md", "# B\n")
	out, _ := m.handleBookmarkView()
	if back := pressed(t, out.(model), tea.KeyPressMsg{Code: '['}); back.currentURL != "mark://host/b.md" || back.status == "bookmarks" {
		t.Errorf("back from bookmarks = %q (%s), want page B", back.currentURL, back.status)
	}
}

const hostile = "evil\x1b]0;owned\x07"

// What a server or a crawled document says reaches the terminal revealed:
// status bar, error view and graph views, not only the rendered body.
func TestServerTextIsRevealedOutsideTheBody(t *testing.T) {
	m := model{ready: true, width: 120, viewport: viewport.New(viewport.WithWidth(120), viewport.WithHeight(10)), linkIdx: -1}
	m.status, m.metadata = "ok"+hostile, map[string]string{"version": "3" + hostile, "modified": hostile}
	views := map[string]string{"status bar": m.statusBarView()}

	m.links, m.linkIdx = []string{"mark://host/" + hostile}, 0
	views["selected link"] = m.statusBarView()

	m.err = fetchErr("refused: " + hostile)
	views["status bar error"], views["error view"] = m.statusBarView(), errorView(m.err)

	m.err, m.viewMode = nil, viewGraph
	m.graphNodes = []graphListItem{{url: "mark://host/a.md", title: hostile, status: "ok", observation: &graph.Observation{}}}
	for _, sub := range []graphSubView{subViewLinks, subViewBacklinks, subViewTopology} {
		m.graphSubView = sub
		views["graph view "+string(rune('0'+sub))] = m.renderCurrentGraphSubView()
	}
	for name, view := range views {
		if strings.Contains(view, "\x1b]0;") || strings.ContainsRune(view, '\x07') || !strings.Contains(view, "<U+001B>") {
			t.Errorf("%s shows the escape raw: %q", name, view)
		}
	}
}

// A link the renderer wraps is clickable on each line it occupies.
func TestWrappedLinkIsClickableOnEveryLine(t *testing.T) {
	body := "Intro words here and [a link whose label is long enough to wrap at this width](target.md) done.\n"
	m := model{ready: true, width: 44, height: 24, styleName: "dark", linkIdx: -1, hoverIdx: -1,
		viewport: viewport.New(viewport.WithWidth(44), viewport.WithHeight(20))}
	m.showDocument(&historyEntry{url: "mark://host/doc.md", rawBody: body, status: "ok"})
	lines := map[int]bool{}
	for _, r := range m.linkRegions {
		if r.Index != 0 || r.EndCol <= r.StartCol {
			t.Fatalf("region = %+v", r)
		}
		lines[r.Line] = true
	}
	if len(lines) < 2 {
		t.Fatalf("regions = %+v, want one per line of the wrapped link", m.linkRegions)
	}
	// A click on the first line follows the link.
	first := m.linkRegions[0]
	out, cmd := m.handleMouseClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: first.StartCol, Y: first.Line + 2})
	if cmd == nil || !out.(model).loading {
		t.Error("a click on the first line of a wrapped link did not follow it")
	}
}
