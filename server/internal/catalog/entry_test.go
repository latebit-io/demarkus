package catalog

import (
	"slices"
	"testing"
	"time"
)

func TestParseTags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "go", []string{"go"}},
		{"multiple", "go,auth,middleware", []string{"go", "auth", "middleware"}},
		{"trims spaces", " go , auth ", []string{"go", "auth"}},
		{"drops empties", "go,,auth,", []string{"go", "auth"}},
		{"only commas", ",,,", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTags(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParseTags(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseImportance(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{"absent", "", defaultImportance},
		{"whitespace", "  ", defaultImportance},
		{"valid mid", "0.9", 0.9},
		{"valid zero", "0", 0},
		{"valid one", "1", 1},
		{"trims", " 0.25 ", 0.25},
		{"unparseable", "high", defaultImportance},
		{"nan", "NaN", defaultImportance},
		{"positive infinity", "+Inf", defaultImportance},
		{"negative", "-0.1", defaultImportance},
		{"above one", "1.5", defaultImportance},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseImportance(tt.in); got != tt.want {
				t.Errorf("ParseImportance(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstH1(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"none", "no heading here\nsecond line", ""},
		{"simple", "# Title\nbody", "Title"},
		{"skips h2", "## Sub\n# Real Title", "Real Title"},
		{"requires space", "#NoSpace\n# Yes Space", "Yes Space"},
		{"leading blank lines", "\n\n# Title", "Title"},
		{"trims heading", "#   Spacey   ", "Spacey"},
		{"first wins", "# First\n# Second", "First"},
		{"skips fenced code", "```\n# install\n```\n# Real Title", "Real Title"},
		{"skips tilde fence", "~~~\n# nope\n~~~\n# Real Title", "Real Title"},
		{"fence with language", "```sh\n# comment\n```\n# Real Title", "Real Title"},
		{"no h1 only fenced", "```\n# install\n```", ""},
		{"skips tab-indented", "\t# tabbed\n# Real Title", "Real Title"},
		{"skips 4-space indented", "    # indented\n# Real Title", "Real Title"},
		{"allows up to 3 spaces", "   # Indented Three", "Indented Three"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstH1([]byte(tt.body)); got != tt.want {
				t.Errorf("firstH1(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestResolveTitle(t *testing.T) {
	tests := []struct {
		name     string
		declared string
		body     string
		path     string
		want     string
	}{
		{"declared wins", "Declared", "# Heading", "/docs/x.md", "Declared"},
		{"falls back to h1", "", "# Heading", "/docs/x.md", "Heading"},
		{"falls back to basename", "", "no heading", "/docs/x.md", "x.md"},
		{"declared trimmed", "  Trimmed  ", "# H", "/x.md", "Trimmed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTitle(tt.declared, []byte(tt.body), tt.path)
			if got != tt.want {
				t.Errorf("resolveTitle(%q, %q, %q) = %q, want %q", tt.declared, tt.body, tt.path, got, tt.want)
			}
		})
	}
}

func TestFromDocument(t *testing.T) {
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	meta := map[string]string{
		"tags":       "go, auth",
		"importance": "0.8",
		"title":      "Auth design",
		"project":    "broker",
	}
	e := FromDocument("/docs/auth.md", meta, []byte("# Ignored\nbody"), mod)

	if !slices.Equal(e.Tags, []string{"go", "auth"}) {
		t.Errorf("tags = %v", e.Tags)
	}
	if e.Importance != 0.8 {
		t.Errorf("importance = %v", e.Importance)
	}
	if e.Title != "Auth design" {
		t.Errorf("title = %q (declared should win over H1)", e.Title)
	}
	if e.Metadata["project"] != "broker" {
		t.Errorf("metadata not preserved: %v", e.Metadata)
	}
	if !e.Modified.Equal(mod) {
		t.Errorf("modified = %v", e.Modified)
	}

	// The metadata map must be copied, not aliased.
	meta["project"] = "mutated"
	if e.Metadata["project"] == "mutated" {
		t.Error("FromDocument aliased the caller's metadata map")
	}
}
