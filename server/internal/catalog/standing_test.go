package catalog

import (
	"testing"
	"time"
)

// TestLookupStandingRanking pins catalog order by importance scaled by
// standing: accepted 1.0, provisional 0.8, retired 0.3; score still first.
func TestLookupStandingRanking(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	c.Set(&Entry{Path: "/accepted.md", Tags: []string{"go", "status:accepted"}, Importance: 0.5, Modified: at})     // 0.50
	c.Set(&Entry{Path: "/untagged.md", Tags: []string{"go"}, Importance: 0.6, Modified: at})                        // 0.48
	c.Set(&Entry{Path: "/wip.md", Tags: []string{"go", "status:wip"}, Importance: 0.55, Modified: at})              // 0.44
	c.Set(&Entry{Path: "/draft.md", Tags: []string{"go", "status:draft"}, Importance: 0.4, Modified: at})           // 0.32
	c.Set(&Entry{Path: "/superseded.md", Tags: []string{"go", "status:superseded"}, Importance: 0.9, Modified: at}) // 0.27
	c.Set(&Entry{Path: "/archived.md", Tags: []string{"go", "status:archived", "auth"}, Importance: 0.1, Modified: at})

	got := paths(mustLookup(t, c, "go", Options{}))
	want := []string{"/accepted.md", "/untagged.md", "/wip.md", "/draft.md", "/superseded.md", "/archived.md"}
	if !equalPaths(got, want) {
		t.Errorf("go = %v, want %v", got, want)
	}
	// A retired document matching more terms still ranks first.
	got = paths(mustLookup(t, c, "go auth", Options{}))
	if len(got) == 0 || got[0] != "/archived.md" {
		t.Errorf("go auth = %v, want /archived.md first", got)
	}
	// Match-all orders by scaled importance too.
	if got = paths(mustLookup(t, c, "*", Options{Max: 2})); !equalPaths(got, want[:2]) {
		t.Errorf("* = %v, want %v", got, want[:2])
	}
}
