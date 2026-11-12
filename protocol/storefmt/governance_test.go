package storefmt

import "testing"

func TestIsGovernancePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/.well-known/demarkus/policy.md":  true,
		"/.well-known//demarkus/x.md":      true,
		".well-known/demarkus/template.md": true,
		"/.WELL-KNOWN/DEMARKUS/policy.md":  true,
		"/a/../.well-known/demarkus/p.md":  true,
		"/.well-known/demarkus":            true,
		"/.well-known/demarkus-other/x.md": false,
		"/.well-known/library/branding.md": false,
		"/docs/.well-known/demarkus/x.md":  false,
	} {
		if got := IsGovernancePath(p); got != want {
			t.Errorf("IsGovernancePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestGeneratedGraphTarget(t *testing.T) {
	for p, want := range map[string]bool{
		"/graph.md":                        true,
		"//graph.md":                       true,
		"/a/../graph/manifest.md":          true,
		"/GRAPH.md":                        true,
		"/Graph/Worlds/team-a/manifest.md": true,
		"/graph/notes.md":                  false,
		"/":                                false,
	} {
		if got := GeneratedGraphTarget(p); got != want {
			t.Errorf("GeneratedGraphTarget(%q) = %v, want %v", p, got, want)
		}
	}
}
