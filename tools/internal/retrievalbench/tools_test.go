package retrievalbench

import (
	"slices"
	"testing"
)

func TestParseLookupRows(t *testing.T) {
	text := "status: ok\nmatches: 2\n\n# Lookup matches for \"x\" in /\n\n| Path | Importance | Title | Tags |\n|------|------------|-------|------|\n| /a.md | 0.9 | A | a,b |\n| /docs/b.md#intro | 0.5 | B | c |\n"
	got := parseLookupRows(text)
	want := []lookupRow{{Path: "/a.md"}, {Path: "/docs/b.md", Anchor: "intro"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := parseLookupRows("status: ok\nmatches: 0\n"); len(got) != 0 {
		t.Fatalf("empty table parsed as %v", got)
	}
}

func TestParseFetchResponse(t *testing.T) {
	outline := parseFetchResponse("status: ok\nversion: 3\nmode: outline\nsize: 9000 bytes, 100 lines\n\n- Doc (#doc, 100 lines)\n")
	if !outline.isOutline() || outline.Header["version"] != "3" {
		t.Fatalf("outline not detected: %+v", outline)
	}
	full := parseFetchResponse("status: ok\nversion: 3\n\n# Doc\n\nintro\n\n## Size Limits\n\nbody\n")
	if full.isOutline() {
		t.Fatal("full body flagged as outline")
	}
	if !full.hasSection("size-limits") || full.hasSection("missing") {
		t.Fatalf("section detection wrong: %+v", full)
	}
}
