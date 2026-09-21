package marktools_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

const exDoc = `# Hub

The hub links everything together.

## Sections

- [Alpha](/alpha.md)
- [Beta](/docs/beta.md)

## More

See [Alpha](/alpha.md) again (deduped).
`

var exListing = []string{"alpha.md", "docs/", "hub.md", "notes.md"}

func exStub() *fetchtest.Client {
	return &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "2", "modified": "2026-07-04T00:00:00Z", "etag": "xyz"},
				Body:     exDoc,
			}}, nil
		},
		ListFn: func(_ context.Context, _ fetch.ListRequest) (fetch.Result, error) {
			return fetchtest.ListPage("/", "", exListing...), nil
		},
	}
}

// exRelations builds the relations arguments as the surface does from a tool
// call: direction defaults to both, page size to ten, and only a query that
// reads incoming edges pays for revalidation.
func exRelations(opts graphstore.NeighborhoodOptions) *marktools.RelationsArgs {
	if opts.Direction == "" {
		opts.Direction = "both"
	}
	if opts.PageSize == 0 {
		opts.PageSize = 10
	}
	return &marktools.RelationsArgs{Options: opts, Revalidate: opts.Direction != graphstore.NeighborhoodOutgoing}
}

func exArgs(url string, relations *marktools.RelationsArgs) marktools.ExploreArgs {
	return marktools.ExploreArgs{URL: url, Render: mcpfmt.Options{Envelope: &mcpfmt.Fetch}, Relations: relations}
}

func exTools(t *testing.T, backend marktools.Backend, store *graphstore.Store) *marktools.Tools {
	t.Helper()
	return (&clientSurface{Store: store}).tools(t, backend)
}

func exText(t *testing.T, tools *marktools.Tools, url string) string {
	t.Helper()
	result := tools.Explore(context.Background(), exArgs(url, nil))
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Text)
	}
	return result.Text
}

func TestExplore_Card(t *testing.T) {
	text := exText(t, exTools(t, exStub(), nil), "mark://host:6309/hub.md")

	wants := []string{
		"status: ok",
		"version: 2",
		"size: ",
		"## Outline",
		"- Hub (#hub,",
		"  - Sections (#sections,",
		"## Opening",
		"The hub links everything together.",
		"## Outbound links (2)",
		"- [Alpha](/alpha.md)",
		"- [Beta](/docs/beta.md)",
		"## Backlinks",
		"(graph store unavailable)",
		"## Siblings in / (3)",
		"- alpha.md",
		"- notes.md",
		"- docs/",
		"fetch mark://host:6309/hub.md#<anchor> for a section",
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("card missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "- hub.md") {
		t.Error("siblings must exclude the document itself")
	}
	// /alpha.md is linked twice in the doc; outbound dedup lists it once.
	if strings.Count(text, "(/alpha.md)") != 1 {
		t.Errorf("outbound links should be deduplicated:\n%s", text)
	}
}

func TestExplore_BacklinksFromStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := graph.New()
	observation := graph.Observe("mark://host/a.md", map[string]string{"version": "2"})
	observation.Complete = true
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Page A", Status: "ok", Observation: observation})
	g.AddNode(&graph.Node{URL: "mark://host:6309/hub.md", Title: "Hub", Status: "ok"})
	g.AddEdge("mark://host:6309/a.md", "mark://host:6309/hub.md")
	gs.Merge(g, nil)

	text := exText(t, exTools(t, exStub(), gs), "mark://host:6309/hub.md")

	// The card shows node identity, which omits the default port (ADR 0005),
	// even though the caller addressed the document by its dial address.
	for _, want := range []string{"## Backlinks (1)", "[Page A](mark://host/a.md)", "freshness: fresh"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected backlink %q in card:\n%s", want, text)
		}
	}
	if strings.Contains(text, "## Relations") || strings.Contains(text, "[source](") {
		t.Fatalf("default card expanded the relation neighborhood:\n%s", text)
	}
}

func TestExplore_OrdinaryReadCachesTypedRelations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	backend := exStub()
	backend.FetchFn = func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"version": "3", "etag": "v3", "rel-supersedes": "/old.md", "rel-depends-on": "/base.md",
			},
			Body: "# Current\n\n[Body](/body.md)\n",
		}}, nil
	}
	result := exTools(t, backend, gs).Explore(context.Background(), exArgs("mark://host:6309/current.md",
		exRelations(graphstore.NeighborhoodOptions{Direction: "outgoing", Relations: []string{"supersedes"}})))
	if result.IsError {
		t.Fatalf("Explore: result=%+v", result)
	}
	text := result.Text
	for _, want := range []string{"## Relations (1 documents)", "mark://host/old.md", "outgoing [supersedes]", "[source](mark://host/current.md)"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "mark://host/base.md") || strings.Contains(text, "mark://host/body.md") {
		t.Errorf("relation filter leaked unmatched rows:\n%s", text)
	}

	reloaded, err := graphstore.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reloaded.Neighborhood("mark://host/current.md", graphstore.NeighborhoodOptions{Direction: graphstore.NeighborhoodOutgoing})
	if err != nil || page.TotalRows != 3 {
		t.Fatalf("cold persisted neighborhood = %+v, %v", page, err)
	}
}

func TestExplore_RelationPagination(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Current\n\n")
	for i := range 5 {
		fmt.Fprintf(&body, "[%02d](/%02d.md)\n", i, i)
	}
	backend := exStub()
	backend.FetchFn = func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "1"}, Body: body.String()}}, nil
	}
	tools := exTools(t, backend, graphstore.New())
	first := tools.Explore(context.Background(), exArgs("mark://host/current.md",
		exRelations(graphstore.NeighborhoodOptions{Direction: "outgoing", PageSize: 2})))
	if first.IsError {
		t.Fatalf("first page: result=%+v", first)
	}
	cursor := exLineValue(first.Text, "next-cursor: ")
	if cursor == "" || !strings.Contains(first.Text, "mark://host/00.md") || strings.Contains(first.Text, "mark://host/02.md") {
		t.Fatalf("first page not bounded:\n%s", first.Text)
	}
	second := tools.Explore(context.Background(), exArgs("mark://host/current.md",
		exRelations(graphstore.NeighborhoodOptions{Direction: "outgoing", PageSize: 2, Cursor: cursor})))
	if second.IsError {
		t.Fatalf("second page: result=%+v", second)
	}
	if !strings.Contains(second.Text, "mark://host/02.md") || strings.Contains(second.Text, "mark://host/00.md") {
		t.Fatalf("second page did not continue:\n%s", second.Text)
	}
}

func exLineValue(text, prefix string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			return value
		}
	}
	return ""
}

func TestExplore_SectionCaps(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Big hub\n\n")
	for i := range 15 {
		fmt.Fprintf(&body, "## Section %d\n\n", i)
		fmt.Fprintf(&body, "- [Doc %d](/doc-%d.md)\n\n", i, i)
	}
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
				Body:     body.String(),
			}}, nil
		},
		ListFn: func(_ context.Context, _ fetch.ListRequest) (fetch.Result, error) {
			return fetchtest.ListPage("/", ""), nil
		},
	}
	text := exText(t, exTools(t, backend, nil), "mark://host:6309/hub.md")

	if !strings.Contains(text, "+6 more headings") {
		t.Errorf("expected heading overflow marker (16 headings, cap 10):\n%s", text)
	}
	if !strings.Contains(text, "+5 more links") {
		t.Errorf("expected link overflow marker (15 links, cap 10):\n%s", text)
	}
	if strings.Contains(text, "Section 12") && strings.Contains(text, "#section-12") {
		t.Error("headings past the cap should not be listed")
	}
}

func TestExplore_ListFailureDegrades(t *testing.T) {
	backend := exStub()
	backend.ListFn = func(_ context.Context, _ fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{}, fmt.Errorf("boom")
	}
	text := exText(t, exTools(t, backend, nil), "mark://host:6309/hub.md")
	if !strings.Contains(text, "(listing unavailable)") {
		t.Errorf("LIST failure should degrade to a note:\n%s", text)
	}
	if !strings.Contains(text, "## Outline") {
		t.Error("card should still render the rest")
	}
}

func TestExplore_NonOKPassthrough(t *testing.T) {
	gs := graphstore.New()
	gs.ObserveDocument("mark://host/missing.md", graph.FetchResult{
		Status: "ok", Body: "# Old\n\n[target](/target.md)", Metadata: map[string]string{"version": "1"},
	})
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusNotFound,
				Metadata: map[string]string{"version": "2"},
			}}, nil
		},
	}
	text := exText(t, exTools(t, backend, gs), "mark://host:6309/missing.md")
	if !strings.Contains(text, "status: not-found") {
		t.Errorf("non-ok fetch should pass through, got:\n%s", text)
	}
	if got := gs.Backlinks("mark://host/target.md"); len(got) != 0 {
		t.Fatalf("confirmed absence retained stale adjacency: %v", got)
	}
}

func TestExplore_BinaryNotice(t *testing.T) {
	gs := graphstore.New()
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1", "modified": "2026-07-04T00:00:00Z", "etag": "abc"},
				Body:     "[false relation](/false.md)\xff",
			}}, nil
		},
	}
	text := exText(t, exTools(t, backend, gs), "mark://host:6309/img.png")
	if !strings.Contains(text, "non-markdown or binary document") {
		t.Errorf("binary body should return the notice, got:\n%s", text)
	}
	if strings.Contains(text, "## Outline") {
		t.Error("binary body must not be run through the outline builder")
	}
	if got := gs.Backlinks("mark://host/false.md"); len(got) != 0 {
		t.Fatalf("binary body cached false relation: %v", got)
	}
}

func TestExplore_BinarySurfacesGraphSaveFailure(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "blocked")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	gs, err := graphstore.Load(filepath.Join(parent, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK, Metadata: map[string]string{"version": "1"}, Body: "\x89PNG\xff",
			}}, nil
		},
	}
	text := exText(t, exTools(t, backend, gs), "mark://host/image.png")
	if !strings.Contains(text, "graph cache save failed") {
		t.Fatalf("binary response hid cache failure:\n%s", text)
	}
}

// exStaleBacklinkStore returns a store where a.md links to hub.md and a.md's
// observation is old enough to be due for revalidation.
func exStaleBacklinkStore(t *testing.T) *graphstore.Store {
	t.Helper()
	gs, err := graphstore.Load(filepath.Join(t.TempDir(), "graph.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := graph.New()
	observation := graph.Observe("mark://host/a.md", map[string]string{"version": "2"})
	observation.Complete = true
	observation.ObservedAt = time.Now().Add(-time.Hour)
	observation.AttemptedAt = observation.ObservedAt
	g.AddNode(&graph.Node{URL: "mark://host/a.md", Title: "Page A", Status: "ok", Observation: observation})
	g.AddNode(&graph.Node{URL: "mark://host/hub.md", Title: "Hub", Status: "ok"})
	g.AddEdge("mark://host/a.md", "mark://host/hub.md")
	gs.Merge(g, nil)
	return gs
}

func TestExplore_DefaultSkipsRevalidation(t *testing.T) {
	backend := exStub()
	var fetched []string
	base := backend.FetchFn
	backend.FetchFn = func(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		fetched = append(fetched, r.Path)
		return base(ctx, r)
	}
	tools := exTools(t, backend, exStaleBacklinkStore(t))

	text := exText(t, tools, "mark://host/hub.md")
	if !strings.Contains(text, "## Backlinks (1)") || !strings.Contains(text, "freshness: stale") {
		t.Fatalf("default card must render cached backlinks with their freshness:\n%s", text)
	}
	if strings.Contains(text, "revalidation:") || slices.Contains(fetched, "/a.md") {
		t.Fatalf("default explore must not revalidate sources; fetched=%v\n%s", fetched, text)
	}

	res := tools.Explore(context.Background(), exArgs("mark://host/hub.md", exRelations(graphstore.NeighborhoodOptions{Direction: "incoming"})))
	if res.IsError {
		t.Fatalf("relation explore: result=%+v", res)
	}
	if !slices.Contains(fetched, "/a.md") {
		t.Fatalf("relation explore must revalidate sources; fetched=%v", fetched)
	}
}
