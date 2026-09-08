package catalog

import (
	"strings"
	"testing"
	"time"
)

func putDoc(c *Catalog, path, body string, meta map[string]string) {
	c.Put(path, meta, []byte(body), time.Now())
}

func locations(rs []Result) string {
	out := make([]string, len(rs))
	for i := range rs {
		out[i] = rs[i].Location()
	}
	return strings.Join(out, ",")
}

// TestReferenceRanking pins the order rules outside the spec: journal paths
// demoted, a tags-or-title match boosted over a mere mention, a
// heading-and-text section over a lower-importance tagged journal.
func TestReferenceRanking(t *testing.T) {
	tests := []struct {
		name  string
		docs  func(c *Catalog)
		query string
		want  string
	}{
		{
			name: "journal path sorts below an equal document elsewhere",
			docs: func(c *Catalog) {
				putDoc(c, "/journal/2026-01-01.md", "# Day\n\nshared phrase kiwi\n", map[string]string{"importance": "0.6"})
				putDoc(c, "/notes.md", "# Notes\n\nshared phrase kiwi\n", map[string]string{"importance": "0.6"})
			},
			query: "kiwi",
			want:  "/notes.md#notes,/journal/2026-01-01.md#day",
		},
		{
			name: "nested journal segment is demoted once",
			docs: func(c *Catalog) {
				putDoc(c, "/sub/journal/2026-01-01.md", "# Day\n\nshared phrase kiwi\n", map[string]string{"importance": "0.6"})
				putDoc(c, "/notes.md", "# Notes\n\nshared phrase kiwi\n", map[string]string{"importance": "0.6"})
			},
			query: "kiwi",
			want:  "/notes.md#notes,/sub/journal/2026-01-01.md#day",
		},
		{
			name: "tagged document outranks a section that only mentions the term",
			docs: func(c *Catalog) {
				putDoc(c, "/plans/mango.md", "# Mango plan\n\n## Goal\n\nRipen the mango.\n", map[string]string{"tags": "mango,fruit", "importance": "0.5"})
				putDoc(c, "/plans/other.md", "# Other\n\n## Mango\n\nA mango is mentioned here and mango again.\n", map[string]string{"importance": "0.9"})
			},
			query: "mango",
			want:  "/plans/mango.md,/plans/other.md#mango",
		},
		{
			name: "heading and text section outranks a lower-importance tagged journal",
			docs: func(c *Catalog) {
				putDoc(c, "/journal/2026-01-02.md", "# Day two\n\nFixed the papaya bug.\n", map[string]string{"tags": "papaya", "importance": "0.55"})
				putDoc(c, "/debugging.md", "# Debugging\n\n## Papaya reverts on restart\n\nThe papaya path is rewritten by the watcher.\n", map[string]string{"tags": "debugging", "importance": "0.85"})
			},
			query: "papaya",
			want:  "/debugging.md#papaya-reverts-on-restart,/journal/2026-01-02.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New()
			tt.docs(c)
			if got := locations(mustLookup(t, c, tt.query, Options{Match: MatchBody})); got != tt.want {
				t.Errorf("body lookup %q = %s, want %s", tt.query, got, tt.want)
			}
		})
	}
}
