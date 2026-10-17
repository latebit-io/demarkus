package graphstore

import (
	"strings"
	"testing"
	"time"
)

func TestBuildWorldsExportRendersRowsTheLibraryParses(t *testing.T) {
	worlds := map[string][]WorldSource{
		"alpha": {
			{Path: "/index.md", Version: 3, Title: "Home", Edges: []WorldEdge{
				{To: "mark://alpha/a.md", Count: 2},
				{To: "mark://soul.example.com/x.md", Label: "soul", Count: 1},
			}},
			{Path: "/a.md", Version: 1, Title: "A"},
		},
		"beta": {{Path: "/b.md", Version: 7, Title: "B", Edges: []WorldEdge{{To: "mark://alpha/a.md", Rel: "supersedes", Count: 1}}}},
	}
	body := BuildWorldsExport(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), worlds)
	nodes, edges, err := ParseExportStrict(body)
	if err != nil {
		t.Fatalf("ParseExportStrict: %v\n%s", err, body)
	}
	if strings.Contains(body, "Source observations") {
		t.Errorf("the export carries source observations:\n%s", body)
	}
	urls := make([]string, 0, len(nodes))
	for i := range nodes {
		if nodes[i].Status != "ok" {
			t.Errorf("node %s status %q, want ok", nodes[i].URL, nodes[i].Status)
		}
		urls = append(urls, nodes[i].URL)
	}
	if want := "mark://alpha/a.md mark://alpha/index.md mark://beta/b.md"; strings.Join(urls, " ") != want {
		t.Errorf("nodes = %v, want %s", urls, want)
	}
	if len(edges) != 3 || edges[2].From != "mark://beta/b.md" || edges[2].Rel != "supersedes" {
		t.Errorf("edges = %+v, want the three row edges sorted by origin", edges)
	}
}

func TestExportContentIgnoresOnlyTheExportedLine(t *testing.T) {
	worlds := map[string][]WorldSource{"alpha": {{Path: "/a.md", Version: 1, Title: "A"}}}
	first := BuildWorldsExport(time.Unix(1, 0), worlds)
	later := BuildWorldsExport(time.Unix(2, 0), worlds)
	if first == later || ExportContent(first) != ExportContent(later) {
		t.Errorf("renders apart in time: content equal %t, bodies equal %t", ExportContent(first) == ExportContent(later), first == later)
	}
	worlds["alpha"][0].Title = "A, renamed"
	if ExportContent(BuildWorldsExport(time.Unix(1, 0), worlds)) == ExportContent(first) {
		t.Error("a renamed node left the content unchanged")
	}
}
