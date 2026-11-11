package merge

import "testing"

// ModeNamed reads a mode by name and nothing else.
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

// A bad value is refused under the name the caller typed, per surface.
func TestModeOfNamesTheSurfacesOption(t *testing.T) {
	for _, raw := range []string{"", "  ", "merge"} {
		if mode, err := ModeOf("-on-conflict", raw); err != nil || mode != OnConflictMerge {
			t.Errorf("ModeOf(%q) = %q, %v; want merge", raw, mode, err)
		}
	}
	if mode, err := ModeOf("on_conflict", " fail "); err != nil || mode != OnConflictFail {
		t.Errorf("ModeOf(fail) = %q, %v", mode, err)
	}
	for _, option := range []string{"-on-conflict", "on_conflict"} {
		_, err := ModeOf(option, "Merge")
		if want := `invalid ` + option + ` "Merge": expected "merge" or "fail"`; err == nil || err.Error() != want {
			t.Errorf("ModeOf(%s) error = %v, want %s", option, err, want)
		}
	}
}
