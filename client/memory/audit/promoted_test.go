package audit

import (
	"context"
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
	got, total, err := Promoted(context.Background(), newFake(docs), "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []PromotedDoc{
		{Path: "/a.md", Markers: promote.Markers{Via: "team", Target: "mark://latebit/a.md", Version: 3}},
		{Path: "/deep/legacy.md", Markers: promote.Markers{Target: "mark://old/legacy.md"}},
		{Path: "/deep/tagged.md"},
	}
	if total != len(docs)-1 || len(got) != len(want) { // the archived one is not listed
		t.Fatalf("Promoted = %+v (%d documents)", got, total)
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
	if _, _, err := Promoted(context.Background(), f, "/", 0); err == nil {
		t.Fatal("a refused listing produced an inventory")
	}
	if _, _, err := Promoted(context.Background(), f, "not a scope", 0); err == nil {
		t.Fatal("an invalid scope produced an inventory")
	}
}
