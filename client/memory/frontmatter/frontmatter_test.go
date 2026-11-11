package frontmatter

import (
	"maps"
	"testing"
)

func TestSplitAndParse(t *testing.T) {
	fm, body, ok := Split([]byte("---\r\nname: Doc\r\ntags:\n  - a\n  - b\n---\n# Doc\n"))
	if !ok || string(body) != "# Doc\n" {
		t.Fatalf("split = %q, %q, %v", fm, body, ok)
	}
	got, err := Parse(fm)
	if want := map[string]string{"name": "Doc", "tags": "a,b"}; err != nil || !maps.Equal(got, want) {
		t.Errorf("parse = %v, %v; want %v", got, err, want)
	}
	if _, _, ok := Split([]byte("---\nname: Doc\n# no closing fence\n")); ok {
		t.Error("an unclosed fence read as frontmatter")
	}
	if _, err := Parse("- orphan"); err == nil {
		t.Error("a list item with no key was accepted")
	}
	if v, bare := Value(`"a: b" # note`); v != "a: b" || bare {
		t.Errorf("Value = %q, %v", v, bare)
	}
}
