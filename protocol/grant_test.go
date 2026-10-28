package protocol

import "testing"

func TestMatchesAnyPath(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{"single match", []string{"/docs/*"}, "/docs/file.md", true},
		{"no recursive match", []string{"/docs/*"}, "/docs/sub/file.md", false},
		{"multiple patterns", []string{"/docs/*", "/public/*"}, "/public/page.md", true},
		{"no match", []string{"/private/*"}, "/docs/file.md", false},
		{"wildcard root", []string{"/*"}, "/anything.md", true},
		{"exact match", []string{"/index.md"}, "/index.md", true},
		{"exact no match", []string{"/index.md"}, "/other.md", false},
		{"empty patterns", []string{}, "/docs/file.md", false},
		// Recursive glob (**) tests.
		{"recursive glob matches child", []string{"/docs/**"}, "/docs/file.md", true},
		{"recursive glob matches nested", []string{"/docs/**"}, "/docs/sub/file.md", true},
		{"recursive glob matches deeply nested", []string{"/docs/**"}, "/docs/a/b/c/file.md", true},
		{"recursive glob no match other dir", []string{"/docs/**"}, "/other/file.md", false},
		{"recursive glob no match prefix itself", []string{"/docs/**"}, "/docs", false},
		{"recursive glob root", []string{"/**"}, "/anything.md", true},
		{"recursive glob root nested", []string{"/**"}, "/a/b/c.md", true},
		{"infix glob matches", []string{"/docs/**/file.md"}, "/docs/sub/file.md", true},
		{"infix glob matches deep", []string{"/docs/**/file.md"}, "/docs/a/b/file.md", true},
		{"infix glob no match wrong suffix", []string{"/docs/**/file.md"}, "/docs/sub/other.md", false},
		{"infix glob no match wrong prefix", []string{"/docs/**/file.md"}, "/other/sub/file.md", false},
		{"infix glob no intermediate dir", []string{"/docs/**/file.md"}, "/docs/file.md", true},
		// Multi-segment suffix after **.
		{"infix glob multi-segment suffix", []string{"/docs/**/sub/*.md"}, "/docs/a/sub/x.md", true},
		{"infix glob multi-segment suffix deep", []string{"/docs/**/sub/*.md"}, "/docs/a/b/sub/notes.md", true},
		{"infix glob multi-segment suffix no match", []string{"/docs/**/sub/*.md"}, "/docs/a/other/x.md", false},
		// Wildcards before **.
		{"wildcard prefix recursive", []string{"/team/*/**"}, "/team/alice/sub/doc.md", true},
		{"wildcard prefix recursive needs the segment", []string{"/team/*/**"}, "/team/doc.md", false},
		{"wildcard prefix infix", []string{"/team/*/**/doc.md"}, "/team/alice/sub/doc.md", true},
		{"wildcard prefix infix no intermediate dir", []string{"/team/*/**/doc.md"}, "/team/alice/doc.md", true},
		{"wildcard prefix infix wrong root", []string{"/team/*/**/doc.md"}, "/other/alice/doc.md", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesAnyPath(tt.patterns, tt.path)
			if got != tt.want {
				t.Errorf("MatchesAnyPath(%v, %q): got %v, want %v",
					tt.patterns, tt.path, got, tt.want)
			}
		})
	}
}
