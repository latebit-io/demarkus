package graphstore

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
)

const neighborhoodCenter = "mark://world/center.md"

func neighborhoodFixture() *Store {
	g := graph.New()
	for url, title := range map[string]string{
		neighborhoodCenter:  "Center",
		"mark://world/a.md": "A",
		"mark://world/b.md": "B",
		"mark://world/z.md": "Z",
	} {
		g.AddNode(&graph.Node{URL: url, Title: title, Status: "ok"})
	}
	g.AddEdgeInfo(graph.Edge{From: "mark://world/z.md", To: neighborhoodCenter, Rel: "references"})
	g.AddEdgeInfo(graph.Edge{From: neighborhoodCenter, To: "mark://world/b.md", Label: "B evidence", Anchor: "proof", Count: 2})
	g.AddEdgeInfo(graph.Edge{From: "mark://world/a.md", To: neighborhoodCenter, Rel: "depends-on"})
	g.AddEdgeInfo(graph.Edge{From: neighborhoodCenter, To: "mark://world/a.md", Rel: "implements"})
	g.AddEdgeInfo(graph.Edge{From: "mark://world/a.md", To: neighborhoodCenter, Label: "plain evidence"})
	store := New()
	store.Merge(g, nil)
	return store
}

func TestNeighborhoodDirectionsGroupDocuments(t *testing.T) {
	store := neighborhoodFixture()
	tests := []struct {
		name      string
		direction NeighborhoodDirection
		wantURLs  []string
		wantEdges []int
	}{
		{name: "incoming", direction: NeighborhoodIncoming, wantURLs: []string{"mark://world/a.md", "mark://world/z.md"}, wantEdges: []int{2, 1}},
		{name: "outgoing", direction: NeighborhoodOutgoing, wantURLs: []string{"mark://world/a.md", "mark://world/b.md"}, wantEdges: []int{1, 1}},
		{name: "both", direction: NeighborhoodBoth, wantURLs: []string{"mark://world/a.md", "mark://world/b.md", "mark://world/z.md"}, wantEdges: []int{3, 1, 1}},
		{name: "default", wantURLs: []string{"mark://world/a.md", "mark://world/b.md", "mark://world/z.md"}, wantEdges: []int{3, 1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: tt.direction})
			if err != nil {
				t.Fatal(err)
			}
			urls := make([]string, len(page.Rows))
			for i := range page.Rows {
				urls[i] = page.Rows[i].Node.URL
				if len(page.Rows[i].Edges) != tt.wantEdges[i] {
					t.Errorf("row %s edges = %d, want %d", urls[i], len(page.Rows[i].Edges), tt.wantEdges[i])
				}
			}
			if !slices.Equal(urls, tt.wantURLs) {
				t.Fatalf("URLs = %v, want %v", urls, tt.wantURLs)
			}
			if page.NextCursor != "" {
				t.Fatalf("NextCursor = %q, want empty", page.NextCursor)
			}
		})
	}
}

func TestNeighborhoodExactRelationFiltersPreservePlain(t *testing.T) {
	store := neighborhoodFixture()
	tests := []struct {
		name      string
		relations []string
		wantURLs  []string
		wantRels  []string
	}{
		{name: "all", relations: nil, wantURLs: []string{"mark://world/a.md", "mark://world/z.md"}, wantRels: []string{"", "depends-on", "references"}},
		{name: "plain", relations: []string{""}, wantURLs: []string{"mark://world/a.md"}, wantRels: []string{""}},
		{name: "typed", relations: []string{"references"}, wantURLs: []string{"mark://world/z.md"}, wantRels: []string{"references"}},
		{name: "union deduped", relations: []string{"depends-on", "", "depends-on"}, wantURLs: []string{"mark://world/a.md"}, wantRels: []string{"", "depends-on"}},
		{name: "empty", relations: []string{}, wantURLs: []string{}, wantRels: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{
				Direction: NeighborhoodIncoming,
				Relations: tt.relations,
			})
			if err != nil {
				t.Fatal(err)
			}
			var urls, relations []string
			for _, row := range page.Rows {
				urls = append(urls, row.Node.URL)
				for _, edge := range row.Edges {
					relations = append(relations, edge.Edge.Rel)
				}
			}
			if !slices.Equal(urls, tt.wantURLs) || !slices.Equal(relations, tt.wantRels) {
				t.Fatalf("URLs/rels = %v/%q, want %v/%q", urls, relations, tt.wantURLs, tt.wantRels)
			}
		})
	}
}

func TestNeighborhoodReturnsIndependentEvidenceCopies(t *testing.T) {
	store := neighborhoodFixture()
	page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodBoth})
	if err != nil {
		t.Fatal(err)
	}
	page.Rows[0].Node.Title = "changed"
	page.Rows[0].Edges[0].Edge.Label = "changed"
	page.Rows[0].Edges[0].Source.Title = "changed"
	page.Rows[0].Edges = nil

	again, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodBoth})
	if err != nil {
		t.Fatal(err)
	}
	if again.Rows[0].Node.Title != "A" || len(again.Rows[0].Edges) != 3 || again.Rows[0].Edges[0].Edge.Label != "plain evidence" || again.Rows[0].Edges[0].Source.Title != "A" {
		t.Fatalf("query result aliases prior result or store: %+v", again.Rows[0])
	}
	if evidence := again.Rows[1].Edges[0]; evidence.Edge.From != neighborhoodCenter || evidence.Edge.To != "mark://world/b.md" || evidence.Edge.Anchor != "proof" || evidence.Edge.Count != 2 {
		t.Fatalf("copied edge lost source evidence: %+v", evidence)
	}
}

func TestNeighborhoodEdgeSourceEvidenceFollowsDirection(t *testing.T) {
	now := time.Now().UTC()
	g := graph.New()
	g.AddNode(&graph.Node{URL: neighborhoodCenter, Title: "Center", Status: "ok", Observation: graph.Observation{
		Source: neighborhoodCenter, View: graph.ViewDocument, Revision: 2, Etag: "center", Complete: true, ObservedAt: now,
	}})
	g.AddNode(&graph.Node{URL: "mark://world/a.md", Title: "A", Status: "ok", Observation: graph.Observation{
		Source: "mark://world/a.md", View: graph.ViewDocument, Revision: 1, Etag: "a", Complete: true, ObservedAt: now,
	}})
	g.AddNode(&graph.Node{URL: "mark://world/b.md", Title: "B", Status: "ok"})
	g.AddEdgeInfo(graph.Edge{From: "mark://world/a.md", To: neighborhoodCenter, Rel: "incoming"})
	g.AddEdgeInfo(graph.Edge{From: neighborhoodCenter, To: "mark://world/b.md", Rel: "outgoing"})
	store := New()
	store.Merge(g, nil)

	page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodBoth})
	if err != nil {
		t.Fatal(err)
	}
	if incoming := page.Rows[0].Edges[0]; incoming.Source.URL != "mark://world/a.md" || incoming.Source.Observation.Revision != 1 {
		t.Fatalf("incoming source = %+v", incoming.Source)
	}
	if outgoing := page.Rows[1].Edges[0]; outgoing.Source.URL != neighborhoodCenter || outgoing.Source.Observation.Revision != 2 {
		t.Fatalf("outgoing source = %+v", outgoing.Source)
	}
}

func TestNeighborhoodCapsSortedRowEvidence(t *testing.T) {
	store := New()
	edges := make([]StoredEdge, 0, MaxNeighborhoodEdgesPerRow+3)
	for i := MaxNeighborhoodEdgesPerRow + 2; i >= 0; i-- {
		edges = append(edges, StoredEdge{
			From: neighborhoodCenter, To: "mark://world/a.md", Rel: fmt.Sprintf("rel-%03d", i), Count: 1,
		})
	}
	store.ReplaceSeed("hub", nil, edges)
	page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodOutgoing})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || len(page.Rows[0].Edges) != MaxNeighborhoodEdgesPerRow || page.Rows[0].OmittedEdges != 3 {
		t.Fatalf("bounded row = %+v", page.Rows)
	}
	for i := range page.Rows[0].Edges {
		evidence := &page.Rows[0].Edges[i]
		if want := fmt.Sprintf("rel-%03d", i); evidence.Edge.Rel != want {
			t.Fatalf("edge %d relation = %q, want %q", i, evidence.Edge.Rel, want)
		}
	}
}

func TestNeighborhoodColdLoadAndWarmReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store.ReplaceSeed("hub", nil, []StoredEdge{{From: "mark://world/old.md", To: neighborhoodCenter, Rel: "references", Count: 1}})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	store, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodIncoming})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Node.URL != "mark://world/old.md" {
		t.Fatalf("cold query = %+v, %v", page, err)
	}

	store.ReplaceSeed("hub", nil, []StoredEdge{{From: "mark://world/new.md", To: neighborhoodCenter, Rel: "references", Count: 1}})
	page, err = store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodIncoming})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Node.URL != "mark://world/new.md" {
		t.Fatalf("warm query = %+v, %v", page, err)
	}
	if got := store.Backlinks(neighborhoodCenter); !slices.Equal(got, []string{"mark://world/new.md"}) {
		t.Fatalf("legacy Backlinks after indexed replacement = %v", got)
	}
}

func TestNeighborhoodPaginationAndCursorIdentity(t *testing.T) {
	store := New()
	edges := make([]StoredEdge, 0, 5)
	for _, name := range []string{"e", "c", "a", "d", "b"} {
		edges = append(edges, StoredEdge{From: neighborhoodCenter, To: "mark://world/" + name + ".md", Count: 1})
	}
	store.ReplaceSeed("hub", nil, edges)
	first, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodOutgoing, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := neighborhoodURLs(first); !slices.Equal(got, []string{"mark://world/a.md", "mark://world/b.md"}) || first.NextCursor == "" {
		t.Fatalf("first page = %v, cursor %q", got, first.NextCursor)
	}
	if strings.Contains(first.NextCursor, "mark://") {
		t.Fatalf("cursor exposes internal position: %q", first.NextCursor)
	}
	second, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{
		Direction: NeighborhoodOutgoing, PageSize: 3, Cursor: first.NextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := neighborhoodURLs(second); !slices.Equal(got, []string{"mark://world/c.md", "mark://world/d.md", "mark://world/e.md"}) || second.NextCursor != "" {
		t.Fatalf("second page = %v, cursor %q", got, second.NextCursor)
	}

	mismatches := []NeighborhoodOptions{
		{Direction: NeighborhoodIncoming, Cursor: first.NextCursor},
		{Direction: NeighborhoodOutgoing, Relations: []string{""}, Cursor: first.NextCursor},
	}
	for _, opts := range mismatches {
		if _, err := store.Neighborhood(neighborhoodCenter, opts); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("mismatched cursor error = %v", err)
		}
	}
	if _, err := store.Neighborhood("mark://world/other.md", NeighborhoodOptions{Direction: NeighborhoodOutgoing, Cursor: first.NextCursor}); err == nil {
		t.Error("cursor accepted for another center")
	}
	if _, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Cursor: "not-base64!"}); err == nil {
		t.Error("malformed cursor accepted")
	}
}

func TestNeighborhoodPageBounds(t *testing.T) {
	store := New()
	edges := make([]StoredEdge, 0, DefaultNeighborhoodPageSize+1)
	for i := range DefaultNeighborhoodPageSize + 1 {
		edges = append(edges, StoredEdge{From: neighborhoodCenter, To: fmt.Sprintf("mark://world/%03d.md", i), Count: 1})
	}
	store.ReplaceSeed("hub", nil, edges)
	page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodOutgoing})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != DefaultNeighborhoodPageSize || page.NextCursor == "" {
		t.Fatalf("default page = %d rows, cursor %q", len(page.Rows), page.NextCursor)
	}
	for _, size := range []int{-1, MaxNeighborhoodPageSize + 1} {
		if _, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{PageSize: size}); err == nil {
			t.Errorf("page size %d accepted", size)
		}
	}
	if _, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: "sideways"}); err == nil {
		t.Error("invalid direction accepted")
	}
}

func TestNeighborhoodConcurrentMutation(t *testing.T) {
	store := New()
	store.ReplaceSeed("hub", nil, []StoredEdge{{From: neighborhoodCenter, To: "mark://world/initial.md", Count: 1}})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			for range 300 {
				page, err := store.Neighborhood(neighborhoodCenter, NeighborhoodOptions{Direction: NeighborhoodBoth, PageSize: MaxNeighborhoodPageSize})
				if err != nil {
					errs <- err
					return
				}
				if err := validateNeighborhoodPage(page); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	for i := range 300 {
		neighbor := fmt.Sprintf("mark://world/%03d.md", i)
		store.ReplaceSeed("hub", nil, []StoredEdge{
			{From: neighborhoodCenter, To: neighbor, Rel: "out", Count: 1},
			{From: neighbor, To: neighborhoodCenter, Rel: "in", Count: 1},
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func neighborhoodURLs(page NeighborhoodPage) []string {
	urls := make([]string, len(page.Rows))
	for i := range page.Rows {
		urls[i] = page.Rows[i].Node.URL
	}
	return urls
}

func validateNeighborhoodPage(page NeighborhoodPage) error {
	for i := range page.Rows {
		row := &page.Rows[i]
		if i > 0 && page.Rows[i-1].Node.URL >= row.Node.URL {
			return fmt.Errorf("rows are not ordered: %q then %q", page.Rows[i-1].Node.URL, row.Node.URL)
		}
		for j := range row.Edges {
			edge := &row.Edges[j].Edge
			if edge.From != neighborhoodCenter && edge.To != neighborhoodCenter {
				return fmt.Errorf("edge is outside neighborhood: %+v", edge)
			}
			neighbor := edge.To
			if edge.To == neighborhoodCenter {
				neighbor = edge.From
			}
			if neighbor != row.Node.URL {
				return fmt.Errorf("edge %+v grouped under %q", edge, row.Node.URL)
			}
		}
	}
	return nil
}
