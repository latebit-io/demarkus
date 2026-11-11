package catalog

import (
	"strings"
	"testing"
	"time"
)

// putDoc publishes one document into the catalog with the given metadata.
func putDoc(c *Catalog, path, body string, meta map[string]string) {
	c.Put(path, meta, []byte(body), time.Now())
}

// locations joins result locations in rank order for one-line assertions.
func locations(rs []Result) string {
	out := make([]string, len(rs))
	for i := range rs {
		out[i] = rs[i].Location()
	}
	return strings.Join(out, ",")
}

// halveJournal is a world's layout prior as its wiring would supply it.
func halveJournal(path string) float64 {
	if strings.Contains(path, "/journal/") {
		return 0.5
	}
	return 1
}

// The catalog demotes no directory by name: without a layout prior a
// journal entry ranks as any other document, with one the world's rule holds.
func TestLayoutPriorIsInjected(t *testing.T) {
	c := New()
	putDoc(c, "/journal/2026-01-01.md", "# Day\n\nshared phrase kiwi\n", map[string]string{"importance": "0.7"})
	putDoc(c, "/notes.md", "# Notes\n\nshared phrase kiwi\n", map[string]string{"importance": "0.6"})
	if got, want := locations(mustLookup(t, c, "kiwi", Options{Match: MatchBody})), "/journal/2026-01-01.md#day,/notes.md#notes"; got != want {
		t.Errorf("without a prior = %s, want %s", got, want)
	}
	if got, want := locations(mustLookup(t, c, "kiwi", Options{Match: MatchBody, Prior: halveJournal})), "/notes.md#notes,/journal/2026-01-01.md#day"; got != want {
		t.Errorf("with the layout prior = %s, want %s", got, want)
	}
}

// TestReferenceRanking pins the order rules outside the spec: journal paths
// (by the injected layout prior) and standing demoted, a tags-or-title match over a mere mention, a
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
			name: "superseded document sorts below a lower-importance draft",
			docs: func(c *Catalog) {
				putDoc(c, "/adr/0001.md", "# Old\n\nshared phrase kiwi\n", map[string]string{"tags": "adr,status:superseded", "importance": "0.9"})
				putDoc(c, "/adr/0002.md", "# New\n\nshared phrase kiwi\n", map[string]string{"tags": "adr,status:draft", "importance": "0.6"})
			},
			query: "kiwi",
			want:  "/adr/0002.md#new,/adr/0001.md#old",
		},
		{
			name: "accepted document outranks an equal untagged one",
			docs: func(c *Catalog) {
				putDoc(c, "/a.md", "# A\n\nshared phrase kiwi\n", map[string]string{"importance": "0.6"})
				putDoc(c, "/b.md", "# B\n\nshared phrase kiwi\n", map[string]string{"tags": "status:accepted", "importance": "0.6"})
			},
			query: "kiwi",
			want:  "/b.md#b,/a.md#a",
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
			if got := locations(mustLookup(t, c, tt.query, Options{Match: MatchBody, Prior: halveJournal})); got != tt.want {
				t.Errorf("body lookup %q = %s, want %s", tt.query, got, tt.want)
			}
		})
	}
}
