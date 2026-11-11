package worldruntime

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/layout"
)

// The demoted directory is the layout's journal, not a name the ranking
// carries: a world that renames it moves the demotion with it.
func TestLayoutPriorFollowsTheLayout(t *testing.T) {
	defaults := layout.Default()
	renamed, err := layout.Parse("journal: daybook/\n")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		lay  *layout.Layout
		path string
		want float64
	}{
		{"default journal", &defaults, "/journal/2026-01-01.md", journalDemote},
		{"default journal nested", &defaults, "/proj/journal/2026-01-01.md", journalDemote},
		{"default elsewhere", &defaults, "/notes/journal.md", 1},
		{"renamed journal", &renamed, "/proj/daybook/2026-01-01.md", journalDemote},
		{"renamed leaves the old name alone", &renamed, "/papers/journal/nature.md", 1},
	}
	for _, tt := range tests {
		if got := LayoutPrior(tt.lay)(tt.path); got != tt.want {
			t.Errorf("%s: prior(%s) = %v, want %v", tt.name, tt.path, got, tt.want)
		}
	}
}
