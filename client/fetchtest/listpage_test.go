package fetchtest

import (
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listing"
)

func TestListPageParsesAsServerPage(t *testing.T) {
	tests := []struct {
		name       string
		next       string
		names      []string
		wantDirs   int
		wantCursor string
	}{
		{name: "empty directory"},
		{name: "files and a directory", names: []string{"a.md", "sub/", "what?#%.md"}, wantDirs: 1},
		{name: "continued page", next: "next", names: []string{"a.md"}, wantCursor: "next"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ListPage("/docs", tt.next, tt.names...)
			meta, err := fetch.ParseListPageMetadata(result.Response)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			if meta.Entries != len(tt.names) || meta.NextCursor != tt.wantCursor {
				t.Errorf("metadata = %+v", meta)
			}
			page, err := listing.ParsePage("/docs", result.Response, "")
			if err != nil {
				t.Fatalf("ParsePage: %v", err)
			}
			dirs := 0
			for _, entry := range page.Entries {
				if entry.IsDir {
					dirs++
				}
			}
			if len(page.Entries) != len(tt.names) || dirs != tt.wantDirs {
				t.Errorf("entries = %+v", page.Entries)
			}
		})
	}
}

func TestResponsesCarryServerMetadata(t *testing.T) {
	if got := Versions("/doc.md", 3).Response.Metadata; got["current"] != "3" || got["total"] != "3" || got["chain-valid"] != "true" {
		t.Errorf("versions metadata = %v", got)
	}
	if got := Lookup("q", "/", "body").Response.Metadata; got["matches"] != "0" || got["match"] != "body" {
		t.Errorf("lookup metadata = %v", got)
	}
	if got := Golden(t, "list").Response.Metadata["complete"]; got != "true" {
		t.Errorf("list golden complete = %q", got)
	}
}
