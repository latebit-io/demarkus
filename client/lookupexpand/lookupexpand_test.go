package lookupexpand

import (
	"context"
	"errors"
	"fmt"
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

// fakeFetch serves docs and records every path asked for.
func fakeFetch(calls *[]string) Fetch {
	return func(_ context.Context, path string) (string, error) {
		*calls = append(*calls, path)
		body, ok := docs[path]
		if !ok {
			return "", errors.New("not-found")
		}
		return body, nil
	}
}

// rowTable renders a catalog-shaped table with the given locations.
func rowTable(paths ...string) string {
	var b strings.Builder
	b.WriteString("| Path | Importance | Title | Tags |\n|---|---|---|---|\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "| %s | 0.9 | T | t |\n", p)
	}
	return b.String()
}

func TestExpandSectionsInOrderWithinBudget(t *testing.T) {
	var calls []string
	out := Expand(context.Background(), table, "text", 10*1024, fakeFetch(&calls))
	for _, want := range []string{">>> /a.md#two\n\n## Two\n\ntwo text", ">>> /b.md\n\n# B\n\nShort whole document.", ">>> /a.md#three\n\n## Three", ">>> note: /missing.md: not-found", ">>> note: expanded 3 of 4 rows"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, ">>> /a.md#two") > strings.Index(out, ">>> /b.md") || strings.Index(out, ">>> /b.md") > strings.Index(out, ">>> /a.md#three") {
		t.Errorf("sections out of rank order:\n%s", out)
	}
	if len(calls) != 3 { // a.md once, b.md once, missing once
		t.Errorf("want one fetch per document, got %v", calls)
	}
}

func TestExpandSkipsSectionsThatDoNotFit(t *testing.T) {
	var calls []string
	two := ">>> /a.md#two\n\n## Two\n\ntwo text\n\n"
	b := ">>> /b.md\n\n# B\n\nShort whole document.\n\n"
	out := Expand(context.Background(), table, "text", len(two)+len(b)+40, fakeFetch(&calls))
	if !strings.Contains(out, two) || !strings.Contains(out, b) {
		t.Errorf("want the first two sections:\n%s", out)
	}
	if strings.Contains(out, ">>> /a.md#three") {
		t.Errorf("third section should not fit:\n%s", out)
	}
	if !strings.Contains(out, ">>> note: expanded 2 of 4 rows") {
		t.Errorf("want the count note:\n%s", out)
	}
}

func TestExpandFitsATinySectionInASmallRemainder(t *testing.T) {
	docs["/t.md"] = "# T\n\n## One\n\n" + strings.Repeat("x", 300) + "\n\n## Two\n\nok\n"
	defer delete(docs, "/t.md")
	var calls []string
	tiny := ">>> /t.md#two\n\n## Two\n\nok\n\n"
	// Budget holds the tiny section but not the 300-byte one ahead of it.
	out := Expand(context.Background(), rowTable("/t.md#one", "/t.md#two"), "x", len(tiny)+5, fakeFetch(&calls))
	if !strings.Contains(out, tiny) || strings.Contains(out, ">>> /t.md#one") {
		t.Errorf("want only the tiny section, decided on its own size:\n%s", out)
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
	out := Expand(context.Background(), rowTable("/big.md"), "nothing matches", 10*1024, fakeFetch(&calls))
	if !strings.Contains(out, ">>> /big.md\n\n- Big (#big") || strings.Contains(out, "filler line\nfiller") {
		t.Errorf("want an outline, not the body:\n%s", out[:min(len(out), 400)])
	}
	out = Expand(context.Background(), rowTable("/big.md"), "Tail end", 10*1024, fakeFetch(&calls))
	if !strings.Contains(out, ">>> /big.md#tail\n\n## Tail\n\nend") || strings.Contains(out, "(#big") {
		t.Errorf("want the term-matching section, not the outline:\n%s", out[:min(len(out), 400)])
	}
}

func TestExpandBoundsFetchesAndHonorsCancel(t *testing.T) {
	paths := make([]string, 0, MaxFetches+3)
	for i := range MaxFetches + 3 {
		paths = append(paths, fmt.Sprintf("/none-%d.md", i))
	}
	var calls []string
	out := Expand(context.Background(), rowTable(paths...), "x", 1<<20, fakeFetch(&calls))
	if len(calls) != MaxFetches || !strings.Contains(out, fmt.Sprintf(">>> note: fetch limit of %d documents reached", MaxFetches)) {
		t.Errorf("want %d fetches then a note, got %d:\n%s", MaxFetches, len(calls), out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = nil
	out = Expand(ctx, table, "x", 1<<20, fakeFetch(&calls))
	if len(calls) != 0 || !strings.Contains(out, ">>> note: stopped: context canceled") {
		t.Errorf("want no fetches after cancel, got %d:\n%s", len(calls), out)
	}
}

func TestExpandMissingSectionAndEmptyInputs(t *testing.T) {
	var calls []string
	out := Expand(context.Background(), rowTable("/a.md#nine"), "x", 4096, fakeFetch(&calls))
	if !strings.Contains(out, ">>> note: /a.md: section #nine not found") {
		t.Errorf("want a missing-section note:\n%s", out)
	}
	if Expand(context.Background(), table, "x", 0, fakeFetch(&calls)) != "" || Expand(context.Background(), "no rows here", "x", 4096, fakeFetch(&calls)) != "" {
		t.Error("zero budget or no rows must expand to nothing")
	}
}
