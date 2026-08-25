package listing

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestParsePage(t *testing.T) {
	page, err := ParsePage("/docs", protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"entries":     "2",
			"complete":    "false",
			"next-cursor": "next",
		},
		Body: "- [a.md](a.md)\n- [sub/](sub/)\n",
	}, "")
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
