package merge

import "testing"

// The store client reads a mode by name and nothing else: what a blank
// option means and how a bad one is refused are the surface's.
func TestModeNamed(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want OnConflict
	}{{"merge", OnConflictMerge}, {" fail ", OnConflictFail}} {
		if got, ok := ModeNamed(tt.raw); !ok || got != tt.want {
			t.Errorf("ModeNamed(%q) = %q, %v; want %q", tt.raw, got, ok, tt.want)
		}
	}
	for _, raw := range []string{"", "  ", "overwrite", "Merge"} {
		if got, ok := ModeNamed(raw); ok || got != "" {
			t.Errorf("ModeNamed(%q) = %q, %v; want no mode", raw, got, ok)
		}
	}
}
