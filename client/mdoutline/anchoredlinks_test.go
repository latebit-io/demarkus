package mdoutline

import (
	"reflect"
	"testing"

	"github.com/latebit-io/demarkus/client/links"
)

func TestAnchoredLinksAttributesSections(t *testing.T) {
	body := "# Title\n\n[intro link](a.md)\n\n## Notes\n\n[first](b.md)\n\n## Notes\n\n[second](c.md)\n\n### Deep\n\n[third](d.md)\n"
	got := AnchoredLinks(body)
	want := []AnchoredLink{
		{Dest: "a.md", Label: "intro link", Anchor: "title"},
		{Dest: "b.md", Label: "first", Anchor: "notes"},
		{Dest: "c.md", Label: "second", Anchor: "notes-1"},
		{Dest: "d.md", Label: "third", Anchor: "deep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AnchoredLinks = %+v, want %+v", got, want)
	}
}

func TestAnchoredLinksAboveFirstHeading(t *testing.T) {
	body := "[early](a.md)\n\n# Title\n\n[late](b.md)\n"
	got := AnchoredLinks(body)
	want := []AnchoredLink{
		{Dest: "a.md", Label: "early", Anchor: ""},
		{Dest: "b.md", Label: "late", Anchor: "title"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AnchoredLinks = %+v, want %+v", got, want)
	}
}

func TestAnchoredLinksEmptyLabel(t *testing.T) {
	body := "# Title\n\n[](a.md)\n\n## Notes\n\n[](b.md)\n"
	got := AnchoredLinks(body)
	want := []AnchoredLink{
		{Dest: "a.md", Label: "", Anchor: "title"},
		{Dest: "b.md", Label: "", Anchor: "notes"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AnchoredLinks = %+v, want %+v", got, want)
	}
}

func TestAnchoredLinksInsideInlineFormatting(t *testing.T) {
	body := "# Title\n\n*see* **[bold](a.md)** and _[italic](b.md)_\n"
	got := AnchoredLinks(body)
	want := []AnchoredLink{
		{Dest: "a.md", Label: "bold", Anchor: "title"},
		{Dest: "b.md", Label: "italic", Anchor: "title"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AnchoredLinks = %+v, want %+v", got, want)
	}
}

func TestAnchoredLinksMatchesExtract(t *testing.T) {
	body := "# Title\n\n[a](a.md) and [a again](a.md#frag)\n\n## Section\n\n[b](b.md) [fragment only](#local)\n"
	anchored := AnchoredLinks(body)
	dests := make([]string, len(anchored))
	for i, l := range anchored {
		dests[i] = l.Dest
	}
	if want := links.Extract(body); !reflect.DeepEqual(dests, want) {
		t.Fatalf("dests = %v, want %v", dests, want)
	}
}
