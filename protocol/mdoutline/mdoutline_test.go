package mdoutline

import (
	"reflect"
	"strings"
	"testing"
)

const sampleDoc = `# Title

Opening paragraph spanning
two lines.

## Setup

Setup body.

### Details

Detail body.

## Usage

Usage body.
`

func TestHeadings(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		levels  []int
		anchors []string
	}{
		{
			"atx tree",
			sampleDoc,
			[]int{1, 2, 3, 2},
			[]string{"title", "setup", "details", "usage"},
		},
		{
			"setext headings",
			"Title\n=====\n\nbody\n\nSection\n-------\n\nmore\n",
			[]int{1, 2},
			[]string{"title", "section"},
		},
		{
			"hash inside code fence is not a heading",
			"# Real\n\n```\n# not a heading\n## also not\n```\n\n## After\n",
			[]int{1, 2},
			[]string{"real", "after"},
		},
		{
			"hash inside tilde fence is not a heading",
			"# Real\n\n~~~sh\n# comment\n~~~\n",
			[]int{1},
			[]string{"real"},
		},
		{
			"hash in indented code block is not a heading",
			"# Real\n\n    # indented code\n",
			[]int{1},
			[]string{"real"},
		},
		{
			"duplicate headings get suffixed anchors",
			"## Notes\n\n## Notes\n\n## Notes\n",
			[]int{2, 2, 2},
			[]string{"notes", "notes-1", "notes-2"},
		},
		{
			"duplicate collides with explicit -1 heading",
			"## Notes\n\n## Notes\n\n## Notes-1\n",
			[]int{2, 2, 2},
			[]string{"notes", "notes-1", "notes-1-1"},
		},
		{
			"empty body",
			"",
			nil,
			nil,
		},
		{
			"no headings",
			"just a paragraph\n",
			nil,
			nil,
		},
		{
			"empty atx heading is skipped",
			"#\n\n## Real\n",
			[]int{2},
			[]string{"real"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := Headings(tt.body)
			var levels []int
			var anchors []string
			for _, h := range hs {
				levels = append(levels, h.Level)
				anchors = append(anchors, h.Anchor)
			}
			if !reflect.DeepEqual(levels, tt.levels) {
				t.Errorf("levels = %v, want %v", levels, tt.levels)
			}
			if !reflect.DeepEqual(anchors, tt.anchors) {
				t.Errorf("anchors = %v, want %v", anchors, tt.anchors)
			}
		})
	}
}

func TestHeadingSpans(t *testing.T) {
	hs := Headings(sampleDoc)
	if len(hs) != 4 {
		t.Fatalf("got %d headings, want 4", len(hs))
	}

	// Title (level 1) spans the whole document.
	if hs[0].Start != 0 || hs[0].End != len(sampleDoc) {
		t.Errorf("title span = [%d, %d), want [0, %d)", hs[0].Start, hs[0].End, len(sampleDoc))
	}
	// Setup (level 2) includes its Details subsection and ends at Usage.
	setup := sampleDoc[hs[1].Start:hs[1].End]
	if !strings.HasPrefix(setup, "## Setup") {
		t.Errorf("setup span starts with %q", setup[:20])
	}
	if !strings.Contains(setup, "### Details") {
		t.Error("setup span should include the Details subsection")
	}
	if strings.Contains(setup, "## Usage") {
		t.Error("setup span should end before Usage")
	}
	// Usage (last section) runs to EOF.
	if hs[3].End != len(sampleDoc) {
		t.Errorf("usage End = %d, want %d", hs[3].End, len(sampleDoc))
	}
}

func TestHeadingLines(t *testing.T) {
	hs := Headings("## A\n\nline1\nline2\n\n## B\nno trailing newline")
	if len(hs) != 2 {
		t.Fatalf("got %d headings, want 2", len(hs))
	}
	if hs[0].Lines != 5 {
		t.Errorf("section A lines = %d, want 5", hs[0].Lines)
	}
	if hs[1].Lines != 2 {
		t.Errorf("section B lines = %d, want 2 (trailing partial line counts)", hs[1].Lines)
	}
}

func TestSlug(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"simple", "Problem", "problem"},
		{"spaces to hyphens", "Problem Statement", "problem-statement"},
		{"punctuation removed", "What's next?", "whats-next"},
		{"em dash removed, double hyphen kept", "W1 — outline mode", "w1--outline-mode"},
		{"slash removed", "outline/section mode", "outlinesection-mode"},
		{"underscores kept", "mark_fetch tool", "mark_fetch-tool"},
		{"hyphens kept", "size-adaptive", "size-adaptive"},
		{"unicode letters kept", "Über uns", "über-uns"},
		{"digits kept", "Phase 2 plan", "phase-2-plan"},
		{"only punctuation", "!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Slug(tt.text); got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestHeadingTextWithInlineCode(t *testing.T) {
	hs := Headings("## The `mark_fetch` tool\n")
	if len(hs) != 1 {
		t.Fatalf("got %d headings, want 1", len(hs))
	}
	if hs[0].Text != "The mark_fetch tool" {
		t.Errorf("text = %q, want backtick content without backticks", hs[0].Text)
	}
	if hs[0].Anchor != "the-mark_fetch-tool" {
		t.Errorf("anchor = %q, want the-mark_fetch-tool", hs[0].Anchor)
	}
}

func TestSection(t *testing.T) {
	tests := []struct {
		name         string
		anchor       string
		wantFound    bool
		wantPrefix   string
		wantContains string
		wantExcludes string
	}{
		{"exact anchor", "setup", true, "## Setup", "### Details", "## Usage"},
		{"leading hash stripped", "#setup", true, "## Setup", "", ""},
		{"case insensitive", "SETUP", true, "## Setup", "", ""},
		{"raw heading text re-slugged", "Opening paragraph", false, "", "", ""},
		{"raw text matches heading", "Details", true, "### Details", "", ""},
		{"last section runs to eof", "usage", true, "## Usage", "Usage body.", ""},
		{"missing anchor", "nope", false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := Section(sampleDoc, tt.anchor)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if !found {
				return
			}
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("section starts with %q, want prefix %q", got[:min(len(got), 30)], tt.wantPrefix)
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("section should contain %q", tt.wantContains)
			}
			if tt.wantExcludes != "" && strings.Contains(got, tt.wantExcludes) {
				t.Errorf("section should not contain %q", tt.wantExcludes)
			}
		})
	}
}

func TestSectionDuplicateAnchors(t *testing.T) {
	body := "## Notes\n\nfirst\n\n## Notes\n\nsecond\n"
	got, found := Section(body, "notes-1")
	if !found {
		t.Fatal("notes-1 not found")
	}
	if !strings.Contains(got, "second") {
		t.Errorf("notes-1 = %q, want the second Notes section", got)
	}
}

func TestOutline(t *testing.T) {
	t.Run("tree shape", func(t *testing.T) {
		got := Outline(sampleDoc)
		want := "- Title (#title, 16 lines)\n" +
			"  - Setup (#setup, 8 lines)\n" +
			"    - Details (#details, 4 lines)\n" +
			"  - Usage (#usage, 3 lines)\n"
		if got != want {
			t.Errorf("outline =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("no headings", func(t *testing.T) {
		if got := Outline("plain text\n"); got != "" {
			t.Errorf("outline = %q, want empty", got)
		}
	})
}

func TestOpeningParagraph(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"after h1", sampleDoc, "Opening paragraph spanning\ntwo lines."},
		{"before any heading", "Intro text.\n\n# Title\n", "Intro text."},
		{"no paragraph", "# Only headings\n\n## Nothing else\n", ""},
		{"empty body", "", ""},
		{"skips list to nothing", "# T\n\n- a list item\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OpeningParagraph(tt.body); got != tt.want {
				t.Errorf("OpeningParagraph = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAnchors(t *testing.T) {
	got := Anchors(sampleDoc)
	want := []string{"title", "setup", "details", "usage"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Anchors = %v, want %v", got, want)
	}
}
