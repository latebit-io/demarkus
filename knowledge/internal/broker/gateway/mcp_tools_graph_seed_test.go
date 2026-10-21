package gateway

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/graph"
	"github.com/latebit-io/demarkus/client/memory/graphstore"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
)

// worldGraphBody renders a /graph.md aggregate for team-a where a.md
// links to b.md.
func worldGraphBody() string {
	src := graphstore.New()
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://team-a/a.md", Title: "Page A", Status: "ok", LinkCount: 1})
	g.AddNode(&graph.Node{URL: "mark://team-a/b.md", Title: "Page B", Status: "ok"})
	g.AddEdgeInfo(graph.Edge{From: "mark://team-a/a.md", To: "mark://team-a/b.md", Label: "B", Count: 1})
	src.Merge(g, nil)
	return src.Export()
}

// seedingDispatcher scripts /graph.md on team-a with the given etag.
func unavailableSeedSource(context.Context, fetch.FetchRequest) (fetch.Result, error) {
	return fetch.Result{}, fmt.Errorf("source unavailable during seed contract test")
}

func seedingDispatcher(etag string) *fakeDispatcher {
	return &fakeDispatcher{
		FetchFn: unavailableSeedSource,
		SeedFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			path, sentEtag := r.Path, r.IfNoneMatch
			if path != "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			if sentEtag != "" && sentEtag == etag {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotModified}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"etag": etag},
				Body:     worldGraphBody(),
			}}, nil
		},
	}
}

func brokerSnapshotRows(t *testing.T, nodes []graphstore.StoredNode, edges []graphstore.StoredEdge) (protocol.Response, map[string]protocol.Response) { //nolint:gocritic // fixture returns manifest and shard set
	t.Helper()
	exported := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	artifacts, err := graphstore.BuildSnapshotShards(graphstore.SnapshotManifestPath, generation.SlotA, nodes, edges, 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]graphstore.SnapshotShardRef, len(artifacts))
	shards := make(map[string]protocol.Response, len(artifacts))
	for i, artifact := range artifacts {
		refs[i] = artifact.Ref(i + 1)
		shards[protocol.VersionPath(artifact.Path, i+1)] = protocol.Response{
			Status: protocol.StatusOK, Body: artifact.Body,
			Metadata: map[string]string{"version": strconv.Itoa(i + 1), "content-hash": artifact.ContentHash},
		}
	}
	manifest, err := graphstore.BuildSnapshotManifest(graphstore.SnapshotManifestPath, graphstore.SnapshotManifest{
		Exported: exported, Complete: true, Nodes: len(nodes), Edges: len(edges),
		ActiveSlot: generation.SlotA, Shards: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Response{Status: protocol.StatusOK, Body: manifest, Metadata: map[string]string{
		"etag": "snapshot-etag-1", "content-hash": generation.BodyHash(manifest),
	}}, shards
}

// deriverContractGoldenPath is the deriver's /graph.md minus its Exported
// line, pinned by federation's TestGraphExportContract; consuming it here is
// the other half of the contract (producer drift regenerates it).
const deriverContractGoldenPath = "../../federation/testdata/graph-export-contract.md"

func deriverContractGolden(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(deriverContractGoldenPath)
	if err != nil {
		t.Fatalf("read export contract golden (regenerate with: UPDATE_GOLDEN=1 go test ./internal/federation -run TestGraphExportContract): %v", err)
	}
	// The golden is ExportContent; the strict parser wants the Exported line back.
	return strings.Replace(string(body), "\n> Nodes:", "\n> Exported: 2026-01-01T00:00:00Z\n> Nodes:", 1)
}

// TestSeedConsumesDeriverExportContract: the deriver's real export (the
// golden) must seed a cold gateway into answering backlink queries by world
// name, typed relations included.
func TestSeedConsumesDeriverExportContract(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "hub", Namespace: "hub"}, core.WorldConfig{Name: "alpha", Namespace: "alpha"})
	body := deriverContractGolden(t)
	d := &fakeDispatcher{
		FetchFn: unavailableSeedSource,
		SeedFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			worldName, path := r.Host, r.Path
			if worldName == "hub" && path == "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: body}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://alpha/docs/a.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	text := toolResultText(t, res)
	// index.md's plain link and b.md's typed rel edge.
	for _, want := range []string{"mark://alpha/index.md", "mark://alpha/docs/b.md", "[supersedes]"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in backlinks from the deriver's real export:\n%s", want, text)
		}
	}
	if node := g.knowledgeGraph.graphStore.GetNode("mark://alpha/docs/a.md"); node == nil || node.Title != "Applications" {
		t.Fatalf("seeded node lost its title: %+v", node)
	}
}

// internalAddrGraphBody renders a /graph.md keyed by internal dial
// addresses (name.namespace.svc.cluster.local:6309), as an outside producer
// might publish it, not by world names.
func internalAddrGraphBody() string {
	src := graphstore.New()
	g := graph.New()
	const base = "mark://team-a.team-a.svc.cluster.local:6309"
	g.AddNode(&graph.Node{URL: base + "/a.md", Title: "Page A", Status: "ok", LinkCount: 2})
	g.AddNode(&graph.Node{URL: base + "/b.md", Title: "Page B", Status: "ok"})
	g.AddNode(&graph.Node{URL: "https://example.com/ext", Status: "external"})
	g.AddEdgeInfo(graph.Edge{From: base + "/a.md", To: base + "/b.md", Label: "B", Count: 1})
	g.AddEdgeInfo(graph.Edge{From: base + "/a.md", To: "https://example.com/ext", Count: 1})
	src.Merge(g, nil)
	return src.Export()
}

// TestSeedTranslatesInternalAddressesToWorldNames: the hub aggregate keys
// rows by cluster-internal DNS, which no legal tool URL can address; seeding
// must translate world addresses to world names and leave unknown hosts alone.
func TestSeedTranslatesInternalAddressesToWorldNames(t *testing.T) {
	d := &fakeDispatcher{
		FetchFn: unavailableSeedSource,
		SeedFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			path := r.Path
			if path != "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: internalAddrGraphBody()}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "Page A") || !strings.Contains(text, "mark://team-a/a.md") {
		t.Errorf("internal-address seed rows not translated to world names:\n%s", text)
	}
	// The external URL is not a world address and must stay as-is, on the
	// node and on the edge destination (asserted via the store; the tool
	// URL grammar only admits mark:// URLs).
	if n := g.knowledgeGraph.graphStore.GetNode("https://example.com/ext"); n == nil {
		t.Error("external node URL was rewritten or dropped")
	}
	if bl := g.knowledgeGraph.graphStore.Backlinks("https://example.com/ext"); len(bl) != 1 || bl[0] != "mark://team-a/a.md" {
		t.Errorf("external edge destination = %v, want [mark://team-a/a.md]", bl)
	}
}

// TestSeedFindsAggregateOnAnotherWorld: the aggregate lives only on the hub
// world, so a cold query against a different world must still seed it; every
// configured world is checked, not just the one in the query URL.
func TestSeedFindsAggregateOnAnotherWorld(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "hub", Namespace: "hub"})
	d := &fakeDispatcher{
		FetchFn: unavailableSeedSource,
		SeedFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			worldName, path := r.Host, r.Path
			if worldName == "hub" && path == "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: internalAddrGraphBody()}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "mark://team-a/a.md") {
		t.Errorf("hub-world aggregate not found from a team-a query:\n%s", text)
	}
	d.Lock()
	defer d.Unlock()
	probed := map[string]int{}
	for _, c := range d.SeedCalls {
		if c.Path != graphstore.SnapshotManifestPath && c.Path != "/graph.md" {
			t.Errorf("seed probed unexpected path %q", c.Path)
		}
		probed[c.Host]++
	}
	if probed["team-a"] != 2 || probed["hub"] != 2 || len(probed) != 2 {
		t.Errorf("seed probes = %v, want snapshot and export checks per world", probed)
	}
}

// TestHandleMarkBacklinksColdPodSeedsFromWorldGraph is the headline
// behavior: a cold gateway answers backlinks from the world's
// published /graph.md with no prior crawl.
func TestHandleMarkBacklinksColdPodSeedsFromWorldGraph(t *testing.T) {
	d := seedingDispatcher("hub-etag-1")
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "Page A") || !strings.Contains(text, "mark://team-a/a.md") {
		t.Errorf("seeded backlink missing:\n%s", text)
	}
	d.Lock()
	defer d.Unlock()
	if len(d.FetchCalls) != 1 {
		t.Errorf("source revalidation fetches = %d, want 1", len(d.FetchCalls))
	}
	if len(d.SeedCalls) != 2 || d.SeedCalls[0].Path != graphstore.SnapshotManifestPath || d.SeedCalls[1].Path != "/graph.md" {
		t.Errorf("fetchCondCalls = %+v, want snapshot then export checks", d.SeedCalls)
	}
}

// Default explore backlinks use the same per-world seed.
func TestHandleMarkExploreBacklinksSeeded(t *testing.T) {
	d := seedingDispatcher("hub-etag-1")
	d.FetchFn = func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		path := r.Path
		if path == "/a.md" {
			return fetch.Result{}, fmt.Errorf("source unavailable")
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# B\n\nBody.\n"}}, nil
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)

	res, err := g.handleMarkExplore(withAliceClaims(context.Background()), callToolReq("mark_explore", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkExplore: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "## Backlinks (1)") || !strings.Contains(text, "Page A") {
		t.Errorf("explore backlinks not seeded:\n%s", text)
	}
}

// A federated world seeds from its checkpoint in the hub, not from its own
// published graph; a world outside the federation never probes the hub.
func TestSeedReadsTheWorldCheckpointFromTheHub(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds[0].Local = true
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "hub", Namespace: "hub"})
	cfg.Federation.Hub = "hub"
	a := graphstore.WorldSource{Path: "/a.md", Version: 3, Title: "Page A", Edges: []graphstore.WorldEdge{{To: "mark://team-a/b.md", Count: 1}}}
	shard, err := graphstore.BuildWorldShard("team-a", graphstore.SourcePrefix(a.Path, 1), []graphstore.WorldSource{a})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := graphstore.BuildWorldManifest(graphstore.WorldManifest{
		World: "team-a", Cursor: protocol.Cursor{Epoch: "e1", Seq: 9}, Complete: true, PrefixLength: 1,
		Shards: []graphstore.WorldShardRef{shard.Ref(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := func(body string) fetch.Result {
		return fetchtest.Head(body, 1, map[string]string{"content-hash": generation.BodyHash(body)})
	}
	d := &fakeDispatcher{
		Published: map[string]fetch.Result{
			"hub" + graphstore.WorldManifestPath("team-a"): doc(manifest),
			"hub" + protocol.VersionPath(shard.Path, 1):    doc(shard.Body),
		},
		FetchFn: unavailableSeedSource,
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "mark://team-a/a.md") || !strings.Contains(text, "Page A") {
		t.Errorf("checkpoint backlink missing:\n%s", text)
	}
	d.Lock()
	defer d.Unlock()
	for _, c := range d.SeedCalls {
		if c.Host == "team-a" {
			t.Errorf("team-a's own graph was read at %s though its checkpoint exists", c.Path)
		}
	}
	for _, c := range d.FetchCalls {
		if c.Host == "hub" && strings.Contains(c.Path, "/graph/worlds/hub/") {
			t.Errorf("the hub, outside the federation here, was probed for a checkpoint at %s", c.Path)
		}
	}
}

// A document published through the gateway and the checkpoint row the
// federation derives for the same version are one source: the backlink the
// checkpoint adds reads fresh, never as a revision conflict.
func TestCheckpointAgreesWithTheGatewaysOwnWriteObservation(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds[0].Local = true
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "hub", Namespace: "hub"})
	cfg.Federation.Hub = "hub"
	body := "# Zeta\n\nSee [the catalog](mark://team-a/index.md) and [alpha](/alpha.md).\n"
	meta := map[string]string{"version": "1", "etag": "etag-v1", "rel-related": "/alpha.md"}
	source, _ := graphstore.DeriveSource("team-a", "/zeta.md", &protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: meta})
	shard, err := graphstore.BuildWorldShard("team-a", graphstore.SourcePrefix(source.Path, 1), []graphstore.WorldSource{source})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := graphstore.BuildWorldManifest(graphstore.WorldManifest{
		World: "team-a", Cursor: protocol.Cursor{Epoch: "e1", Seq: 2}, Complete: true, PrefixLength: 1,
		Shards: []graphstore.WorldShardRef{shard.Ref(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := func(body string) fetch.Result {
		return fetchtest.Head(body, 1, map[string]string{"content-hash": generation.BodyHash(body)})
	}
	d := &fakeDispatcher{
		Published: map[string]fetch.Result{
			"hub" + graphstore.WorldManifestPath("team-a"): doc(manifest),
			"hub" + protocol.VersionPath(shard.Path, 1):    doc(shard.Body),
		},
		FetchFn: unavailableSeedSource,
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url": "mark://team-a/zeta.md", "body": body, "expected_version": float64(0),
		"metadata": map[string]any{"rel-related": "/alpha.md"},
	}))
	if err != nil || res.IsError {
		t.Fatalf("handleMarkPublish = (%+v, %v)", res, err)
	}
	res, err = g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{"url": "mark://team-a/alpha.md"}))
	if err != nil || res.IsError {
		t.Fatalf("handleMarkBacklinks = (%+v, %v)", res, err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "mark://team-a/zeta.md") {
		t.Fatalf("backlink missing:\n%s", text)
	}
	if strings.Contains(text, "revision-conflict") || !strings.Contains(text, "freshness: fresh") {
		t.Fatalf("the write and its checkpoint disagree:\n%s", text)
	}
}

// A checkpoint row seeded with no local observation to mask it records the
// world's address as its source, the form every gateway observation uses.
func TestCheckpointRowsRecordTheWorldsAddressAsSource(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds[0].Local = true
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "hub", Namespace: "hub"})
	cfg.Federation.Hub = "hub"
	row := graphstore.WorldSource{Path: "/zeta.md", Version: 1, Title: "Zeta", Edges: []graphstore.WorldEdge{{To: "mark://team-a/alpha.md", Count: 1}}}
	shard, err := graphstore.BuildWorldShard("team-a", graphstore.SourcePrefix(row.Path, 1), []graphstore.WorldSource{row})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := graphstore.BuildWorldManifest(graphstore.WorldManifest{
		World: "team-a", Cursor: protocol.Cursor{Epoch: "e1", Seq: 1}, Complete: true, PrefixLength: 1,
		Shards: []graphstore.WorldShardRef{shard.Ref(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := func(body string) fetch.Result {
		return fetchtest.Head(body, 1, map[string]string{"content-hash": generation.BodyHash(body)})
	}
	d := &fakeDispatcher{
		Published: map[string]fetch.Result{
			"hub" + graphstore.WorldManifestPath("team-a"): doc(manifest),
			"hub" + protocol.VersionPath(shard.Path, 1):    doc(shard.Body),
		},
		FetchFn: unavailableSeedSource,
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	if _, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{"url": "mark://team-a/alpha.md"})); err != nil {
		t.Fatal(err)
	}
	node := g.knowledgeGraph.graphStore.GetNode("mark://team-a/zeta.md")
	want := links.NodeURL(cfg.Worlds[0].Address(), "/zeta.md")
	if node == nil || node.Observation.Source != want {
		t.Fatalf("seeded row source = %+v, want %q", node, want)
	}
}
