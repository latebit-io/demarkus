package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/promote"
)

func TestPromotedInventoriesMarkersAndTagsOverTheWholeScope(t *testing.T) {
	docs := map[string]fakeDoc{
		"/index.md":       tagged("# Hub\n\n- [a](a.md)\n"),
		"/a.md":           tagged("# A\n\npromoted-via: team\npromoted: mark://latebit/a.md@v3\n\nBody.\n"),
		"/b.md":           tagged("# B\n\nNot promoted.\n"),
		"/deep/legacy.md": tagged("# Legacy\n\npromoted: mark://old/legacy.md\n\nBody.\n"),
		"/deep/tagged.md": {Body: "# Tagged\n\nBody.\n", Meta: map[string]string{"tags": "x," + promote.Tag}},
		"/gone.md":        {Body: "# Gone\n\npromoted: mark://x/g.md@v1\n", Status: "archived"},
	}
	inv, err := Promoted(context.Background(), newFake(docs), "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, total := inv.Docs, inv.Documents
	want := []PromotedDoc{
		{Path: "/a.md", Markers: promote.Markers{Via: "team", Target: "mark://latebit/a.md", Version: 3}},
		{Path: "/deep/legacy.md", Markers: promote.Markers{Target: "mark://old/legacy.md"}},
		{Path: "/deep/tagged.md"},
	}
	if total != len(docs)-1 || len(got) != len(want) || len(inv.Coverage) != 0 { // the archived one is not listed
		t.Fatalf("Promoted = %+v (%d documents, coverage %q)", got, total, inv.Coverage)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("doc %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPromotedFailsOnAnUntrustworthyInventory(t *testing.T) {
	f := newFake(map[string]fakeDoc{"/index.md": tagged("# Hub\n")})
	f.listStatus["/"] = "unauthorized"
	if _, err := Promoted(context.Background(), f, "/", 0); err == nil {
		t.Fatal("a refused listing produced an inventory")
	}
	if _, err := Promoted(context.Background(), f, "not a scope", 0); err == nil {
		t.Fatal("an invalid scope produced an inventory")
	}
}

// A document that listed but did not read is a gap the inventory names, not
// a document that is silently not promoted.
func TestPromotedReportsAnUnreadDocument(t *testing.T) {
	inv, err := Promoted(context.Background(), newFake(map[string]fakeDoc{
		"/index.md": tagged("# Hub\n"),
		"/a.md":     {Body: "# A\n\npromoted: mark://x/a.md@v1\n", Status: "not-found"},
	}), "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Docs) != 0 || len(inv.Coverage) != 1 || !strings.Contains(inv.Coverage[0], "/a.md") {
		t.Fatalf("Promoted = %+v", inv)
	}
}
