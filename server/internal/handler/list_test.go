package handler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
)

func TestListCursorRoundTripAndScope(t *testing.T) {
	cursor, err := encodeListCursor("/docs", true, "guide.md")
	if err != nil {
		t.Fatalf("encodeListCursor: %v", err)
	}
	after, err := decodeListCursor(cursor, "/docs", true)
	if err != nil || after != "guide.md" {
		t.Fatalf("decodeListCursor = (%q, %v), want (guide.md, nil)", after, err)
	}
	if _, err := decodeListCursor(cursor, "/other", true); err == nil {
		t.Error("cursor accepted for another directory")
	}
	if _, err := decodeListCursor(cursor, "/docs", false); err == nil {
		t.Error("archived cursor accepted for live listing")
	}
	if _, err := decodeListCursor("not-base64!", "/docs", true); err == nil {
		t.Error("malformed cursor accepted")
	}
}

func TestParseListPageSize(t *testing.T) {
	if got, err := parseListPageSize(""); err != nil || got != MaxDirectoryEntries {
		t.Fatalf("default = (%d, %v)", got, err)
	}
	for _, invalid := range []string{"0", "1001", "nope"} {
		if _, err := parseListPageSize(invalid); err == nil {
			t.Errorf("page size %q accepted", invalid)
		}
	}
}

func TestBuildDirectoryPage(t *testing.T) {
	entries := []storefmt.DirEntry{
		{Name: "a.md"},
		{Name: "b", IsDir: true},
		{Name: "c.md"},
	}
	first, err := buildDirectoryPage("/docs", entries, 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Complete || first.EntryCount != 2 || first.LastName != "b" ||
		!strings.Contains(first.Body, "[a.md]") || !strings.Contains(first.Body, "[b/]") ||
		!strings.Contains(first.Body, "truncated") {
		t.Fatalf("first page = %+v\n%s", first, first.Body)
	}
	second, err := buildDirectoryPage("/docs", entries[2:], 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if !second.Complete || second.EntryCount != 1 || second.LastName != "c.md" ||
		strings.Contains(second.Body, "truncated") || !strings.Contains(second.Body, "[c.md]") {
		t.Fatalf("second page = %+v\n%s", second, second.Body)
	}
}

func TestBuildDirectoryPageBoundsBody(t *testing.T) {
	entries := make([]storefmt.DirEntry, 1000)
	for i := range entries {
		entries[i].Name = strings.Repeat("[]", 2000) + string(rune(0x1000+i))
	}
	page, err := buildDirectoryPage("/", entries, 1000)
	if err != nil {
		t.Fatalf("buildDirectoryPage: %v", err)
	}
	if page.Complete || page.EntryCount == 0 {
		t.Fatalf("page = %+v, want progressing incomplete page", page)
	}
	if len(page.Body) > protocol.MaxBodyLength {
		t.Fatalf("body bytes = %d, max %d", len(page.Body), protocol.MaxBodyLength)
	}
}

func TestBuildDirectoryPageRejectsOrderingDrift(t *testing.T) {
	_, err := buildDirectoryPage("/", []storefmt.DirEntry{{Name: "b"}, {Name: "a"}}, 10)
	if err == nil || errors.Is(err, errListPageCannotProgress) {
		t.Fatalf("ordering error = %v", err)
	}
}

// windowRecorder notes every listing window the handler asks a view for.
type windowRecorder struct {
	storagebackend.ReadView
	windows *[]storefmt.ListOptions
}

func (v *windowRecorder) ListEntries(ctx context.Context, reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	*v.windows = append(*v.windows, opts)
	return v.ReadView.ListEntries(ctx, reqPath, opts)
}

// A page asks the backend for page-size plus one entries past the cursor,
// never for the whole directory.
func TestListReadsOnlyTheWindow(t *testing.T) {
	b := fileBackend(t)
	seedBackend(t, b, map[string]string{
		"docs/a.md": "# A\n", "docs/b.md": "# B\n", "docs/c.md": "# C\n", "docs/d.md": "# D\n", "docs/e.md": "# E\n",
	})
	var windows []storefmt.ListOptions
	b.Store = &viewStore{DocumentStore: b.Store, wrap: func(view storagebackend.ReadView) storagebackend.ReadView {
		return &windowRecorder{ReadView: view, windows: &windows}
	}}
	h := newHandler(b, nil)

	first := newMockStream("LIST /docs/\n---\npage-size: 2\n---\n")
	h.HandleStream(context.Background(), first)
	firstResp, err := protocol.ParseResponse(&first.output)
	if err != nil {
		t.Fatalf("parse first response: %v", err)
	}
	if firstResp.Metadata["entries"] != "2" || firstResp.Metadata["complete"] != "false" {
		t.Fatalf("first page metadata = %+v", firstResp.Metadata)
	}
	second := newMockStream("LIST /docs/\n---\npage-size: 2\ncursor: " + firstResp.Metadata["next-cursor"] + "\n---\n")
	h.HandleStream(context.Background(), second)

	want := []storefmt.ListOptions{{Limit: 3}, {After: "b.md", Limit: 3}}
	if !slices.Equal(windows, want) {
		t.Errorf("windows = %+v, want %+v", windows, want)
	}
}
