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
		if _, err := StrategyByName("lookup-fetch", "/", tt.limit, tt.fetches); err == nil {
			t.Fatalf("limit=%d fetches=%d accepted", tt.limit, tt.fetches)
		}
	}
	if _, err := StrategyByName("lookup-fetch", "/", 10, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := StrategyByName("nope", "/", 10, 5); err == nil {
		t.Fatal("unknown strategy accepted")
	}
}
