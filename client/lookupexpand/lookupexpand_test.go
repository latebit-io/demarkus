package lookupexpand

import (
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mdoutline"
)

const table = "| Path | Importance | Title | Tags | Snippet |\n|---|---|---|---|---|\n" +
	"| /a.md#two | 0.9 | A › Two | a | snippet |\n" +
	"| /b.md | 0.8 | B | b | |\n" +
	"| /a.md#three | 0.7 | A › Three | a | snippet |\n" +
	"| /missing.md#x | 0.5 | M | m | |\n"

var docs = map[string]string{
	"/a.md": "# A\n\nIntro.\n\n## One\n\none text\n\n## Two\n\ntwo text\n\n## Three\n\n" + strings.Repeat("three text ", 30) + "\n",
	"/b.md": "# B\n\nShort whole document.\n",
}

func fakeFetch(calls *[]string) Fetch {
	return func(path string) (string, error) {
		*calls = append(*calls, path)
		body, ok := docs[path]
		if !ok {
			return "", errors.New("not-found")
		}
		return body, nil
	}
}

func TestExpandSectionsInOrderWithinBudget(t *testing.T) {
	var calls []string
	out := Expand(table, "text", 10*1024, fakeFetch(&calls))
	for _, want := range []string{"## /a.md#two\n\n## Two\n\ntwo text", "## /b.md\n\n# B\n\nShort whole document.", "## /a.md#three\n\n## Three", "note: /missing.md: not-found", "note: expanded 3 of 4 rows"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "## /a.md#two") > strings.Index(out, "## /b.md") || strings.Index(out, "## /b.md") > strings.Index(out, "## /a.md#three") {
		t.Errorf("sections out of rank order:\n%s", out)
	}
	if len(calls) != 3 { // a.md once, b.md once, missing once
		t.Errorf("want one fetch per document, got %v", calls)
	}
}

func TestExpandSkipsSectionsThatDoNotFit(t *testing.T) {
	var calls []string
	// Room for the first section and the b.md body, not for both plus three.
	out := Expand(table, "text", len("## /a.md#two\n\n## Two\n\ntwo text\n\n")+len("## /b.md\n\n# B\n\nShort whole document.\n\n")+minRemaining, fakeFetch(&calls))
	if !strings.Contains(out, "## /a.md#two") || !strings.Contains(out, "## /b.md") {
		t.Errorf("want the first two sections:\n%s", out)
	}
	if strings.Contains(out, "## /a.md#three") {
		t.Errorf("third section should not fit:\n%s", out)
	}
	if !strings.Contains(out, "note: expanded 2 of 4 rows") {
		t.Errorf("want the count note:\n%s", out)
	}
}

func TestExpandOutlinesLargeBarePath(t *testing.T) {
	big := "# Big\n\nIntro.\n\n" + strings.Repeat("filler line\n\n", 800) + "## Tail\n\nend\n"
	docs["/big.md"] = big
	defer delete(docs, "/big.md")
	if len(big) < mdoutline.OutlineThreshold {
		t.Fatal("fixture must be over the threshold")
	}
	var calls []string
	row := "| Path | Importance | Title | Tags |\n|---|---|---|---|\n| /big.md | 0.9 | Big | b |\n"
	out := Expand(row, "nothing matches", 10*1024, fakeFetch(&calls))
	if !strings.Contains(out, "## /big.md\n\n- Big (#big") || strings.Contains(out, "filler line\nfiller") {
		t.Errorf("want an outline, not the body:\n%s", out[:min(len(out), 400)])
	}
	out = Expand(row, "Tail end", 10*1024, fakeFetch(&calls))
	if !strings.Contains(out, "## /big.md#tail\n\n## Tail\n\nend") || strings.Contains(out, "(#big") {
		t.Errorf("want the term-matching section, not the outline:\n%s", out[:min(len(out), 400)])
	}
}

func TestExpandMissingSectionAndEmptyInputs(t *testing.T) {
	var calls []string
	out := Expand("| Path | Importance | Title | Tags |\n|---|---|---|---|\n| /a.md#nine | 0.9 | A | a |\n", "x", 4096, fakeFetch(&calls))
	if !strings.Contains(out, "note: /a.md: section #nine not found") {
		t.Errorf("want a missing-section note:\n%s", out)
	}
	if Expand(table, "x", 0, fakeFetch(&calls)) != "" || Expand("no rows here", "x", 4096, fakeFetch(&calls)) != "" {
		t.Error("zero budget or no rows must expand to nothing")
	}
}
