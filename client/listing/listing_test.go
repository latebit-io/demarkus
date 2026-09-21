package listing

import (
	"slices"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestParsePage(t *testing.T) {
	page, err := ParsePage("/docs", RenderPage("/docs", []Entry{{Name: "a.md"}, {Name: "sub", IsDir: true}}, "next"), "")
	if err != nil || len(page.Entries) != 2 || page.LastName != "sub" || page.Complete || page.NextCursor != "next" {
		t.Fatalf("page = (%+v, %v)", page, err)
	}
	if page.Entries[1] != (Entry{Name: "sub", Path: "/docs/sub", IsDir: true}) {
		t.Errorf("entry = %+v", page.Entries[1])
	}
}

func TestParsePageReportsInvalidAndOrdering(t *testing.T) {
	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"entries": "2", "complete": "true"},
		Body:     "- [bad](../bad.md)\n- [b.md](b.md)\n",
	}
	page, err := ParsePage("/docs", resp, "a.md")
	if err != nil || len(page.Invalid) != 1 || len(page.Entries) != 1 {
		t.Fatalf("page = (%+v, %v)", page, err)
	}
	resp.Body = "- [b.md](b.md)\n- [a.md](a.md)\n"
	if _, err := ParsePage("/docs", resp, ""); err == nil {
		t.Fatal("ordering drift accepted")
	}
}

// RenderPage is the producer fakes use; it must parse back unchanged,
// awkward names and a continuation cursor included.
func TestRenderPageRoundTrip(t *testing.T) {
	entries := []Entry{
		{Name: "a b_[c].md", Path: "/docs/a b_[c].md"},
		{Name: "sub", Path: "/docs/sub", IsDir: true},
		{Name: "z#1.md", Path: "/docs/z#1.md"},
	}
	for _, cursor := range []string{"", "cursor-1"} {
		page, err := ParsePage("/docs", RenderPage("/docs", entries, cursor), "")
		if err != nil {
			t.Fatalf("cursor %q: %v", cursor, err)
		}
		if len(page.Invalid) != 0 || !slices.Equal(page.Entries, entries) {
			t.Errorf("cursor %q: entries = %+v invalid = %v", cursor, page.Entries, page.Invalid)
		}
		if page.NextCursor != cursor || page.Complete != (cursor == "") {
			t.Errorf("cursor %q: next = %q complete = %v", cursor, page.NextCursor, page.Complete)
		}
	}
}
