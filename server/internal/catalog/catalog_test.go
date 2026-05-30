package catalog

import (
	"testing"
	"time"
)

// paths returns the result paths in order, for concise assertions.
func paths(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Path
	}
	return out
}

func equalPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSetRemoveLen(t *testing.T) {
	c := New()
	if c.Len() != 0 {
		t.Fatalf("new catalog len = %d", c.Len())
	}
	c.Set(&Entry{Path: "/a.md", Tags: []string{"go"}})
	c.Set(&Entry{Path: "/b.md", Tags: []string{"go"}})
	if c.Len() != 2 {
		t.Fatalf("len after 2 sets = %d", c.Len())
	}
	// Set on the same path replaces, not appends.
	c.Set(&Entry{Path: "/a.md", Tags: []string{"rust"}})
	if c.Len() != 2 {
		t.Fatalf("len after replace = %d", c.Len())
	}
	c.Remove("/a.md")
	if c.Len() != 1 {
		t.Fatalf("len after remove = %d", c.Len())
	}
	c.Remove("/missing.md") // no-op
	if c.Len() != 1 {
		t.Fatalf("len after removing missing = %d", c.Len())
	}
}

func TestLookupMatching(t *testing.T) {
	c := New()
	c.Set(&Entry{Path: "/tagged.md", Tags: []string{"auth", "go"}, Title: "Unrelated"})
	c.Set(&Entry{Path: "/titled.md", Tags: []string{"misc"}, Title: "Auth Middleware Design"})
	c.Set(&Entry{Path: "/none.md", Tags: []string{"misc"}, Title: "Unrelated"})

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"tag match", "auth", []string{"/tagged.md", "/titled.md"}}, // title substring also matches
		{"title substring", "middleware", []string{"/titled.md"}},
		{"case insensitive", "AUTH", []string{"/tagged.md", "/titled.md"}},
		{"no match", "kubernetes", nil},
		{"empty query", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := paths(c.Lookup(tt.query, Options{}))
			if !equalPaths(got, tt.want) {
				t.Errorf("Lookup(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestLookupRanking(t *testing.T) {
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	// Matches one term, high importance.
	c.Set(&Entry{Path: "/one-term.md", Tags: []string{"auth"}, Importance: 0.99, Modified: recent})
	// Matches both terms — score should win over importance.
	c.Set(&Entry{Path: "/two-terms.md", Tags: []string{"auth", "middleware"}, Importance: 0.1, Modified: old})
	// Same score (1) as one-term but lower importance.
	c.Set(&Entry{Path: "/also-one.md", Tags: []string{"middleware"}, Importance: 0.5, Modified: recent})

	got := paths(c.Lookup("auth middleware", Options{}))
	want := []string{"/two-terms.md", "/one-term.md", "/also-one.md"}
	if !equalPaths(got, want) {
		t.Errorf("ranking = %v, want %v", got, want)
	}
}

func TestLookupRankingTiebreaks(t *testing.T) {
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	// All score 1 and importance 0.5: newer first, then path ascending.
	c.Set(&Entry{Path: "/b.md", Tags: []string{"go"}, Importance: 0.5, Modified: recent})
	c.Set(&Entry{Path: "/a.md", Tags: []string{"go"}, Importance: 0.5, Modified: recent})
	c.Set(&Entry{Path: "/c.md", Tags: []string{"go"}, Importance: 0.5, Modified: old})

	got := paths(c.Lookup("go", Options{}))
	want := []string{"/a.md", "/b.md", "/c.md"} // a,b newer (path tiebreak), c oldest last
	if !equalPaths(got, want) {
		t.Errorf("tiebreak = %v, want %v", got, want)
	}
}

func TestLookupScope(t *testing.T) {
	c := New()
	c.Set(&Entry{Path: "/docs/a.md", Tags: []string{"go"}})
	c.Set(&Entry{Path: "/docs/sub/b.md", Tags: []string{"go"}})
	c.Set(&Entry{Path: "/other/c.md", Tags: []string{"go"}})

	tests := []struct {
		name  string
		scope string
		want  []string
	}{
		{"root", "/", []string{"/docs/a.md", "/docs/sub/b.md", "/other/c.md"}},
		{"empty is root", "", []string{"/docs/a.md", "/docs/sub/b.md", "/other/c.md"}},
		{"subtree", "/docs/", []string{"/docs/a.md", "/docs/sub/b.md"}},
		{"subtree no trailing slash", "/docs", []string{"/docs/a.md", "/docs/sub/b.md"}},
		{"deeper subtree", "/docs/sub", []string{"/docs/sub/b.md"}},
		{"no prefix collision", "/oth", nil}, // must not match /other by raw prefix
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := paths(c.Lookup("go", Options{Scope: tt.scope}))
			if !equalPaths(got, tt.want) {
				t.Errorf("scope %q = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}

func TestLookupMax(t *testing.T) {
	c := New()
	for _, p := range []string{"/a.md", "/b.md", "/c.md", "/d.md"} {
		c.Set(&Entry{Path: p, Tags: []string{"go"}, Importance: 0.5})
	}
	got := c.Lookup("go", Options{Max: 2})
	if len(got) != 2 {
		t.Fatalf("Max=2 returned %d results", len(got))
	}
	// Max 0 means no cap.
	if all := c.Lookup("go", Options{Max: 0}); len(all) != 4 {
		t.Fatalf("Max=0 returned %d results, want 4", len(all))
	}
}

func TestLookupWithFilter(t *testing.T) {
	c := New()
	c.Set(&Entry{Path: "/broker.md", Tags: []string{"go"}, Metadata: map[string]string{"project": "broker"}})
	c.Set(&Entry{Path: "/universe.md", Tags: []string{"go"}, Metadata: map[string]string{"project": "universe"}})

	preds, err := ParseFilter("project=broker")
	if err != nil {
		t.Fatal(err)
	}
	got := paths(c.Lookup("go", Options{Filter: preds}))
	want := []string{"/broker.md"}
	if !equalPaths(got, want) {
		t.Errorf("filtered lookup = %v, want %v", got, want)
	}
}

func TestLookupScorePopulated(t *testing.T) {
	c := New()
	c.Set(&Entry{Path: "/two.md", Tags: []string{"auth", "middleware"}})
	got := c.Lookup("auth middleware", Options{})
	if len(got) != 1 || got[0].Score != 2 {
		t.Fatalf("expected one result with score 2, got %+v", got)
	}
}
