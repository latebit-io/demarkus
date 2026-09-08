package retrievalbench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeTools serves a canned catalog: lookup returns rows in order, fetch
// returns the doc body, an outline for big docs, or a section.
type fakeTools struct {
	rows []string
	docs map[string]string // path -> body
	big  map[string]bool
	fail error
}

type wordCounter struct{}

func (wordCounter) Name() string                { return "words" }
func (wordCounter) Count(s string) (int, error) { return len(strings.Fields(s)), nil }

func (f fakeTools) Call(_ context.Context, name string, args map[string]any) (ToolResult, error) {
	if f.fail != nil {
		return ToolResult{}, f.fail
	}
	switch name {
	case "mark_lookup":
		var b strings.Builder
		b.WriteString("status: ok\n\n| Path | Importance | Title | Tags |\n|---|---|---|---|\n")
		for _, r := range f.rows {
			fmt.Fprintf(&b, "| %s | 0.5 | t | a |\n", r)
		}
		return ToolResult{Text: b.String()}, nil
	case "mark_fetch":
		url, _ := args["url"].(string)
		path, anchor, _ := strings.Cut(url, "#")
		body, ok := f.docs[path]
		if !ok {
			return ToolResult{Text: "status: not-found\n", IsError: true}, nil
		}
		force, _ := args["force"].(bool)
		switch {
		case anchor != "":
			parsed := parseFetchResponse("status: ok\n\n" + body)
			if !parsed.hasSection(anchor) {
				return ToolResult{Text: "section not found", IsError: true}, nil
			}
			return ToolResult{Text: "status: ok\nsection: #" + anchor + "\n\n## " + anchor + "\n\nsection body words\n"}, nil
		case f.big[path] && !force:
			return ToolResult{Text: "status: ok\nmode: outline\n\n- outline\n"}, nil
		default:
			return ToolResult{Text: "status: ok\n\n" + body}, nil
		}
	}
	return ToolResult{}, fmt.Errorf("unexpected tool %s", name)
}

func TestLookupFetch(t *testing.T) {
	docs := map[string]string{
		"/a.md": "# A\n\n## Intro\n\ntext\n",
		"/b.md": "# B\n\n## Ranking\n\nsorted\n",
		"/c.md": "# C\n\ntext\n",
		"/d.md": "# D\n\n## Threats\n\nlist\n",
	}
	strategy := LookupFetch{Scope: "/", LookupLimit: 10, MaxFetches: 5}
	tests := []struct {
		name     string
		tools    fakeTools
		q        Question
		wantHit  bool
		wantCall int // calls to evidence
		wantN    int // total calls
		wantRank int
		wantErr  bool
	}{
		{
			name:    "rank one small doc with anchor",
			tools:   fakeTools{rows: []string{"/b.md", "/a.md"}, docs: docs},
			q:       Question{ExpectedPath: "/b.md", ExpectedAnchor: "ranking"},
			wantHit: true, wantCall: 2, wantN: 2, wantRank: 1,
		},
		{
			name:    "rank three big doc needs section fetch",
			tools:   fakeTools{rows: []string{"/a.md", "/c.md", "/d.md"}, docs: docs, big: map[string]bool{"/d.md": true}},
			q:       Question{ExpectedPath: "/d.md", ExpectedAnchor: "threats"},
			wantHit: true, wantCall: 5, wantN: 5, wantRank: 3,
		},
		{
			name:    "big doc no anchor forces full body",
			tools:   fakeTools{rows: []string{"/d.md"}, docs: docs, big: map[string]bool{"/d.md": true}},
			q:       Question{ExpectedPath: "/d.md"},
			wantHit: true, wantCall: 3, wantN: 3, wantRank: 1,
		},
		{
			name:    "small doc lacking anchor is a miss",
			tools:   fakeTools{rows: []string{"/c.md"}, docs: docs},
			q:       Question{ExpectedPath: "/c.md", ExpectedAnchor: "threats"},
			wantHit: false, wantN: 2, wantRank: 1,
		},
		{
			name:    "absent from lookup burns the fetch budget",
			tools:   fakeTools{rows: []string{"/a.md", "/b.md", "/c.md", "/d.md", "/a.md", "/b.md"}, docs: docs},
			q:       Question{ExpectedPath: "/zzz.md"},
			wantHit: false, wantN: 6, wantRank: 0,
		},
		{
			name:    "rank beyond budget is a miss",
			tools:   fakeTools{rows: []string{"/a.md", "/b.md", "/c.md", "/a.md", "/b.md", "/d.md"}, docs: docs},
			q:       Question{ExpectedPath: "/d.md"},
			wantHit: false, wantN: 6, wantRank: 6,
		},
		{
			name:    "outline at the fifth fetch leaves no call for the section",
			tools:   fakeTools{rows: []string{"/a.md", "/b.md", "/c.md", "/a.md", "/d.md"}, docs: docs, big: map[string]bool{"/d.md": true}},
			q:       Question{ExpectedPath: "/d.md", ExpectedAnchor: "threats"},
			wantHit: false, wantN: 6, wantRank: 5,
		},
		{
			name:    "transport failure surfaces",
			tools:   fakeTools{fail: errors.New("boom")},
			q:       Question{ExpectedPath: "/a.md"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := NewRecorder(tt.tools, wordCounter{})
			out, err := strategy.Run(context.Background(), rec, &tt.q)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.Hit != tt.wantHit || out.CallsToEvidence != tt.wantCall || out.LookupRank != tt.wantRank {
				t.Fatalf("outcome %+v, want hit=%v calls=%d rank=%d", out, tt.wantHit, tt.wantCall, tt.wantRank)
			}
			if n := len(rec.Calls()); n != tt.wantN {
				t.Fatalf("recorded %d calls, want %d", n, tt.wantN)
			}
			for _, c := range rec.Calls() {
				if c.Tokens == 0 {
					t.Fatalf("call %s recorded zero tokens", c.Tool)
				}
			}
		})
	}
}

func TestStrategyByNameRejectsBudgets(t *testing.T) {
	for _, tt := range []struct{ limit, fetches int }{{0, 5}, {10, 0}, {-1, -1}} {
		if _, err := StrategyByName("lookup-fetch", "/", tt.limit, tt.fetches, 1500); err == nil {
			t.Fatalf("limit=%d fetches=%d accepted", tt.limit, tt.fetches)
		}
	}
	if _, err := StrategyByName("lookup-fetch", "/", 10, 5, 1500); err != nil {
		t.Fatal(err)
	}
	if _, err := StrategyByName("nope", "/", 10, 5, 1500); err == nil {
		t.Fatal("unknown strategy accepted")
	}
}

// bodyFakeTools serves a body-mode table (path#anchor rows with a snippet)
// when match=body is requested, else the catalog table; nobody flags a
// server that ignores the key and answers from the catalog.
type bodyFakeTools struct {
	fakeTools
	bodyRows []string // path#anchor or bare path
	nobody   bool
}

func (f *bodyFakeTools) Call(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if name != "mark_lookup" || args["match"] != "body" || f.nobody {
		return f.fakeTools.Call(ctx, name, args)
	}
	var b strings.Builder
	b.WriteString("status: ok\nmatch: body\n\n| Path | Importance | Title | Tags | Snippet |\n|---|---|---|---|---|\n")
	for _, r := range f.bodyRows {
		fmt.Fprintf(&b, "| %s | 0.5 | t › h | a | snippet words |\n", r)
	}
	return ToolResult{Text: b.String()}, nil
}

func TestBodyFetch(t *testing.T) {
	docs := map[string]string{
		"/a.md": "# A\n\n## Intro\n\ntext\n",
		"/b.md": "# B\n\n## Ranking\n\nsorted\n\n### Ties\n\npath order\n",
		"/d.md": "# D\n\n## Threats\n\nlist\n",
	}
	strategy := LookupFetch{Scope: "/", LookupLimit: 5, MaxFetches: 5, Body: true}
	tests := []struct {
		name     string
		tools    bodyFakeTools
		q        Question
		wantHit  bool
		wantCall int
		wantN    int
		wantRank int
	}{
		{
			name:    "top row at the named anchor",
			tools:   bodyFakeTools{fakeTools: fakeTools{docs: docs}, bodyRows: []string{"/b.md#ranking", "/a.md#intro"}},
			q:       Question{ExpectedPath: "/b.md", ExpectedAnchor: "ranking"},
			wantHit: true, wantCall: 2, wantN: 2, wantRank: 1,
		},
		{
			name:    "decoy first then the target section",
			tools:   bodyFakeTools{fakeTools: fakeTools{docs: docs}, bodyRows: []string{"/a.md#intro", "/d.md#threats"}},
			q:       Question{ExpectedPath: "/d.md", ExpectedAnchor: "threats"},
			wantHit: true, wantCall: 3, wantN: 3, wantRank: 2,
		},
		{
			name:    "sibling section of the target document is a miss",
			tools:   bodyFakeTools{fakeTools: fakeTools{docs: docs}, bodyRows: []string{"/b.md#ties"}},
			q:       Question{ExpectedPath: "/b.md", ExpectedAnchor: "ranking"},
			wantHit: false, wantN: 2, wantRank: 1,
		},
		{
			name:    "sibling section first, the named section next",
			tools:   bodyFakeTools{fakeTools: fakeTools{docs: docs}, bodyRows: []string{"/b.md#ties", "/b.md#ranking"}},
			q:       Question{ExpectedPath: "/b.md", ExpectedAnchor: "ranking"},
			wantHit: true, wantCall: 3, wantN: 3, wantRank: 1,
		},
		{
			name:    "bare row on a big document costs the outline round trip",
			tools:   bodyFakeTools{fakeTools: fakeTools{docs: docs, big: map[string]bool{"/d.md": true}}, bodyRows: []string{"/d.md"}},
			q:       Question{ExpectedPath: "/d.md", ExpectedAnchor: "threats"},
			wantHit: true, wantCall: 3, wantN: 3, wantRank: 1,
		},
		{
			name:    "server without body match falls back to catalog rows",
			tools:   bodyFakeTools{fakeTools: fakeTools{docs: docs, rows: []string{"/b.md"}}, nobody: true},
			q:       Question{ExpectedPath: "/b.md", ExpectedAnchor: "ranking"},
			wantHit: true, wantCall: 2, wantN: 2, wantRank: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := NewRecorder(&tt.tools, wordCounter{})
			out, err := strategy.Run(context.Background(), rec, &tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if out.Hit != tt.wantHit || out.CallsToEvidence != tt.wantCall || out.LookupRank != tt.wantRank {
				t.Fatalf("outcome %+v, want hit=%v calls=%d rank=%d", out, tt.wantHit, tt.wantCall, tt.wantRank)
			}
			if n := len(rec.Calls()); n != tt.wantN {
				t.Fatalf("recorded %d calls, want %d", n, tt.wantN)
			}
			if rec.Calls()[0].Args["match"] != "body" {
				t.Fatalf("lookup args = %v, want match body", rec.Calls()[0].Args)
			}
		})
	}
}

func TestStrategyByNameBodyFetch(t *testing.T) {
	s, err := StrategyByName("body-fetch", "/x/", 5, 3, 1500)
	if err != nil || s.Name() != "body-fetch" {
		t.Fatalf("StrategyByName body-fetch = %v, %v", s, err)
	}
	if _, err := StrategyByName("body-fetch", "/", 0, 3, 1500); err == nil {
		t.Fatal("zero lookup limit accepted")
	}
}

func TestExpansionHolds(t *testing.T) {
	q := &Question{ExpectedPath: "/a.md", ExpectedAnchor: "two"}
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"anchored block", "| /a.md#two | 0.9 | A | a | s |\n\n>>> /a.md#two\n\n## Two\n\ntext\n\n>>> note: expanded 1 of 1 rows within the budget\n", true},
		{"other anchor", "\n>>> /a.md#three\n\n## Three\n\ntext\n\n>>> note: expanded 1 of 1 rows\n", false},
		{"whole document holds the heading", "\n>>> /a.md\n\n# A\n\n## One\n\n## Two\n\ntext\n\n>>> note: expanded 1 of 1 rows\n", true},
		{"outline block holds nothing", "\n>>> /a.md\n\n- A (#a, 9 lines)\n  - Two (#two, 3 lines)\n\n>>> note: expanded 1 of 1 rows\n", false},
		{"other path", "\n>>> /b.md#two\n\n## Two\n\ntext\n\n>>> note: expanded 1 of 1 rows\n", false},
		{"markdown heading naming the path is body, not a frame", "\n>>> /b.md#two\n\n## /a.md#two\n\n## Two\n\ntext\n\n>>> note: expanded 1 of 1 rows\n", false},
		{"note-like body line does not end a block", "\n>>> /a.md\n\n# A\n\nnote: keep reading\n\n## Two\n\ntext\n\n>>> note: expanded 1 of 1 rows\n", true},
		{"table only", "| /a.md#two | 0.9 | A | a | s |\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expansionHolds(c.text, q); got != c.want {
				t.Errorf("expansionHolds = %v, want %v", got, c.want)
			}
		})
	}
	if !expansionHolds("\n>>> /a.md#nine\n\ntext\n\n>>> note: x\n", &Question{ExpectedPath: "/a.md"}) {
		t.Error("no expected anchor: any section of the path is evidence")
	}
}
