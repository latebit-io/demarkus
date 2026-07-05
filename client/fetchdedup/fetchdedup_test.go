package fetchdedup

import (
	"strings"
	"testing"
)

func TestIdentified(t *testing.T) {
	tests := []struct {
		name string
		doc  Doc
		want bool
	}{
		{"both fields", Doc{Version: "3", Etag: "a"}, true},
		{"version only", Doc{Version: "3"}, true},
		{"etag only", Doc{Etag: "a"}, true},
		{"neither", Doc{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.doc.Identified(); got != tt.want {
				t.Errorf("Identified() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnchangedNotice(t *testing.T) {
	tests := []struct {
		name    string
		doc     Doc
		wantIn  []string
		wantOut []string
	}{
		{
			"versioned",
			Doc{Version: "3", Etag: "abc"},
			[]string{"status: unchanged\n", "version: 3\n", "etag: abc\n", "unchanged since v3", "force=true"},
			nil,
		},
		{
			"etag-only identity",
			Doc{Etag: "abc"},
			[]string{"status: unchanged\n", "etag: abc\n", "unchanged since this session's earlier fetch (etag match)"},
			[]string{"version:", "since v"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UnchangedNotice(tt.doc)
			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("notice missing %q in:\n%s", want, got)
				}
			}
			for _, not := range tt.wantOut {
				if strings.Contains(got, not) {
					t.Errorf("notice should not contain %q:\n%s", not, got)
				}
			}
		})
	}
}

// TestChangedNote pins the identity-delta wording, including the
// asymmetric versioned<->etag-only flips that must not render a bare
// "v" with no number.
func TestChangedNote(t *testing.T) {
	tests := []struct {
		name string
		prev Doc
		cur  Doc
		want string
	}{
		{"version bump", Doc{Version: "3"}, Doc{Version: "5"}, "changed since this session's earlier fetch (v3 -> v5)"},
		{"was etag-only, now versioned", Doc{Etag: "a"}, Doc{Version: "5"}, "changed since this session's earlier fetch (was etag-only, now v5)"},
		{"was versioned, now etag-only", Doc{Version: "3"}, Doc{Etag: "b"}, "changed since this session's earlier fetch (was v3, now etag-only)"},
		{"etag rotated, version steady", Doc{Version: "3", Etag: "a"}, Doc{Version: "3", Etag: "b"}, "content changed since this session's earlier fetch (still v3, etag differs)"},
		{"etag-only identity", Doc{Etag: "a"}, Doc{Etag: "b"}, "content changed since this session's earlier fetch (etag differs)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ChangedNote(tt.prev, tt.cur); got != tt.want {
				t.Errorf("ChangedNote = %q, want %q", got, tt.want)
			}
		})
	}
}
