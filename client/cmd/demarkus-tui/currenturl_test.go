package main

import (
	"path/filepath"
	"testing"

	"charm.land/bubbles/v2/textinput"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/internal/bookmarks"
	"github.com/latebit-io/demarkus/protocol"
)

func shownDocument(t *testing.T, url string) model {
	t.Helper()
	store, err := bookmarks.Load(filepath.Join(t.TempDir(), "bookmarks.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := model{addressBar: textinput.New(), bookmarkStore: store, histIdx: -1} // as initialModel starts
	m.addressBar.SetValue(url)
	out, _ := m.handleFetchResult(fetchResult{
		url: url, seq: m.fetchSeq,
		result: fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Shown\n"}},
	})
	return out.(model)
}

// The address bar is an input: once edited it no longer names what is on
// screen. Actions on "this document" act on the one that was fetched.
func TestBookmarkTogglesTheShownDocumentNotTheTypedText(t *testing.T) {
	m := shownDocument(t, "mark://host/shown.md")
	m.addressBar.SetValue("mark://host/typed-but-never-fetched.md")

	out, _ := m.handleBookmarkToggle()
	m = out.(model)
	if !m.bookmarkStore.Has("mark://host/shown.md") || m.bookmarkStore.Has("mark://host/typed-but-never-fetched.md") {
		t.Fatalf("bookmarks = %+v, want the shown document", m.bookmarkStore.List())
	}
}

func TestAFailedFetchLeavesNoCurrentDocument(t *testing.T) {
	m := shownDocument(t, "mark://host/shown.md")
	m.addressBar.SetValue("mark://host/missing.md")
	out, _ := m.handleFetchResult(fetchResult{url: "mark://host/missing.md", seq: m.fetchSeq, err: errBoom})
	m = out.(model)
	if m.currentURL != "" {
		t.Errorf("currentURL = %q after a failed fetch, want none: the old page is gone from the screen", m.currentURL)
	}
	if out, _ := m.handleBookmarkToggle(); len(out.(model).bookmarkStore.List()) != 0 {
		t.Error("bookmarked something with no document on screen")
	}
}

var errBoom = fetchErr("boom")

type fetchErr string

func (e fetchErr) Error() string { return string(e) }
