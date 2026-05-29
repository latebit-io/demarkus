package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/client/graph"
	"github.com/latebit/demarkus/protocol"
	"k8s.io/client-go/kubernetes/fake"
)

// crawlBody helps tests script doc bodies the graph crawler will
// follow links from. Returns markdown with a title + a list of
// links, matching what the links package extracts.
func crawlBody(title string, dests ...string) string {
	var b strings.Builder
	if title != "" {
		b.WriteString("# " + title + "\n\n")
	}
	for _, d := range dests {
		b.WriteString("- [link](" + d + ")\n")
	}
	return b.String()
}

func TestHandleMarkBacklinksEmptyStoreHintsAtMarkGraph(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/foo.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if res.IsError {
		t.Errorf("isError = true on empty store (should be a textual hint, not an error): %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	for _, want := range []string{"No backlinks", "mark_graph", "ephemeral"} {
		if !strings.Contains(text, want) {
			t.Errorf("hint text missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkBacklinksMissingURL(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", nil))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing url, want true")
	}
}

func TestHandleMarkBacklinksInvalidURL(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "https://wrong-scheme/foo",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on https:// URL, want true")
	}
}

// TestHandleMarkBacklinksAfterCrawlReturnsLinkedDocs runs a real
// crawl through mark_graph, then queries backlinks on a doc the
// crawl discovered — exercises the full graph store path:
// CrawlAndPersist writes to the broker's ephemeral store,
// BacklinksEnriched reads it.
func TestHandleMarkBacklinksAfterCrawlReturnsLinkedDocs(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			switch path {
			case "/index.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody("Index", "/about.md", "/contact.md"),
				}}, nil
			case "/about.md", "/contact.md":
				// Both link back to /index.md — that's the
				// backlink the test asserts on.
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody(strings.TrimPrefix(path, "/"), "/index.md"),
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	ctx := withAliceClaims(context.Background())

	// Populate the ephemeral store.
	if res, err := g.handleMarkGraph(ctx, callToolReq("mark_graph", map[string]any{
		"url": "mark://team-a/index.md",
	})); err != nil || res.IsError {
		t.Fatalf("mark_graph: err=%v isError=%v text=%s", err, res.IsError, toolResultText(t, res))
	}

	// Query backlinks for /index.md — should find /about.md
	// and /contact.md.
	res, err := g.handleMarkBacklinks(ctx, callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if res.IsError {
		t.Fatalf("backlinks isError after crawl: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	for _, want := range []string{"about.md", "contact.md", "Backlinks for mark://team-a/index.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("backlinks text missing %q\nfull:\n%s", want, text)
		}
	}
}

// TestEphemeralGraphStoreResetsAcrossGatewayInstances pins the
// load-bearing property of the ephemeral design: a new
// mcpGateway gets a fresh empty store, simulating a broker pod
// restart. If we ever switch to a persistent store, this test
// changes shape (or moves to a "persistence enabled" branch).
func TestEphemeralGraphStoreResetsAcrossGatewayInstances(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   crawlBody("only", "/other.md"),
			}}, nil
		},
	}

	g1 := newGatewayWithDispatcher(t, cfg, d)
	if res, err := g1.handleMarkGraph(withAliceClaims(context.Background()), callToolReq("mark_graph", map[string]any{
		"url": "mark://team-a/seed.md",
	})); err != nil || res.IsError {
		t.Fatalf("first crawl: %v %v", err, res.IsError)
	}
	if g1.graphStore.NodeCount() == 0 {
		t.Fatal("first crawl produced an empty graph; cannot exercise the reset property")
	}

	// Fresh gateway — simulates a broker pod restart. Even
	// using the SAME server config, the new mcpGateway
	// instance must have an empty graph store.
	g2 := newGatewayWithDispatcher(t, cfg, d)
	if got := g2.graphStore.NodeCount(); got != 0 {
		t.Errorf("new gateway's graph store has %d nodes, want 0 (ephemeral semantics broken)", got)
	}

	// Backlinks query on the new gateway returns the empty
	// hint, NOT the result from the previous gateway.
	res, err := g2.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/seed.md",
	}))
	if err != nil || res.IsError {
		t.Fatalf("second-gateway backlinks: %v %v", err, res.IsError)
	}
	if !strings.Contains(toolResultText(t, res), "No backlinks") {
		t.Errorf("expected empty-store hint after restart; got:\n%s", toolResultText(t, res))
	}
}

func TestHandleMarkGraphMissingURL(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkGraph(withAliceClaims(context.Background()), callToolReq("mark_graph", nil))
	if err != nil {
		t.Fatalf("handleMarkGraph: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing url, want true")
	}
}

func TestHandleMarkGraphDepthClamping(t *testing.T) {
	// Plan: depth clamps to [1, 5]. depth=0 clamps to 1 (only
	// the seed + its direct neighbors crawl); depth=99 clamps to
	// 5 (the deeper graph crawls). Multi-hop graph is required
	// to actually observe the clamp — without it, the test
	// would pass even if depth were silently ignored.
	//
	// Graph shape: seed.md → child.md → grandchild.md →
	//              greatgrandchild.md
	// depth=1 reaches seed + child (2 crawled nodes, 1 edge).
	// depth=5 reaches everything (4 crawled nodes, 3 edges).
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			switch path {
			case "/seed.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody("seed", "/child.md"),
				}}, nil
			case "/child.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody("child", "/grandchild.md"),
				}}, nil
			case "/grandchild.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody("grandchild", "/greatgrandchild.md"),
				}}, nil
			case "/greatgrandchild.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody("greatgrandchild"),
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}

	// depth=0 clamps to 1 — seed + its direct neighbors crawl.
	// The crawler records EDGES to deeper targets (child → grandchild)
	// even though grandchild itself is never fetched, so a
	// string-presence test on the output is misleading. The
	// load-bearing property is node count: how many docs did we
	// actually fetch and walk into the graph store?
	gShallow := newGatewayWithDispatcher(t, cfg, d)
	if shallowRes, sherr := gShallow.handleMarkGraph(withAliceClaims(context.Background()), callToolReq("mark_graph", map[string]any{
		"url":   "mark://team-a/seed.md",
		"depth": float64(0),
	})); sherr != nil || shallowRes.IsError {
		t.Fatalf("shallow crawl: err=%v isError=%v", sherr, shallowRes.IsError)
	}

	// depth=99 clamps to 5 — full 4-node chain crawls.
	gDeep := newGatewayWithDispatcher(t, cfg, d)
	if deepRes, dherr := gDeep.handleMarkGraph(withAliceClaims(context.Background()), callToolReq("mark_graph", map[string]any{
		"url":   "mark://team-a/seed.md",
		"depth": float64(99),
	})); dherr != nil || deepRes.IsError {
		t.Fatalf("deep crawl: err=%v isError=%v", dherr, deepRes.IsError)
	}

	// The crawler creates nodes for every URL it touches, whether
	// it fetches them or just records them as edge destinations.
	// graph.Crawl with MaxDepth=N enqueues children of depth-(N-1)
	// nodes but doesn't enqueue deeper. So:
	//   shallow (MaxDepth=1): seed (depth 0) crawls; child (depth 1)
	//   added as a node but not enqueued. → 2 nodes total.
	//   deep (MaxDepth=5):   all 4 of the chain crawl, plus the
	//   greatgrandchild's "no link" body adds no extras. → 4 nodes.
	//
	// If depth were silently ignored both gateways would produce
	// the same NodeCount (likely the deep count since the full
	// chain would be reachable). The strict inequality is what
	// pins the clamp behavior.
	if gShallow.graphStore.NodeCount() == gDeep.graphStore.NodeCount() {
		t.Errorf("shallow node count (%d) == deep node count (%d) — depth not producing different traversal scopes",
			gShallow.graphStore.NodeCount(), gDeep.graphStore.NodeCount())
	}
	if gShallow.graphStore.NodeCount() >= gDeep.graphStore.NodeCount() {
		t.Errorf("shallow node count (%d) should be < deep node count (%d)",
			gShallow.graphStore.NodeCount(), gDeep.graphStore.NodeCount())
	}
}

// TestHandleMarkIndexBoundsOnDirectoryCycle pins the cycle-
// detection guard in walkIndexDir. PR #149 review surfaced the
// DoS shape: a LIST that links a subdirectory back to its
// ancestor would have recursed forever before this fix.
// maxIndexDocuments caps file appends but not the directory
// traversal itself, so without visited-set protection an
// adversarial (or buggy) world could pin the broker on a
// single index call.
func TestHandleMarkIndexBoundsOnDirectoryCycle(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "hub",
		Namespace:    "hub",
		TokensSecret: "hub-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/*"},
		},
	})
	var listCalls atomic.Int32
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			if strings.HasSuffix(path, protocol.WellKnownManifestPath) {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   indexManifestBody,
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		listFn: func(_, _, _ string) (fetch.Result, error) {
			n := listCalls.Add(1)
			// Sanity cap: if the fix is broken, this will fire
			// thousands of times before any test timeout. Fail
			// loud at 100 LIST calls so the test fails in
			// seconds rather than hanging.
			if n > 100 {
				t.Errorf("walkIndexDir called LIST %d times on a cyclic graph — cycle detection regressed", n)
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			// Pathological LIST body: a subdirectory link that
			// points back to the parent. Without cycle
			// detection this would recurse indefinitely.
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   "- [self](./)\n- [parent](../)\n",
			}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkIndex(withAliceClaims(context.Background()), callToolReq("mark_index", map[string]any{
		"source": "mark://team-a/",
		"target": "mark://hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkIndex: %v", err)
	}
	if res.IsError {
		// Acceptable if the cycle scenario produces an error
		// envelope, as long as we didn't recurse indefinitely.
		// What's NOT acceptable is a hang or runaway LIST count.
		t.Logf("index returned tool error (acceptable for cycle scenario): %s", toolResultText(t, res))
	}
	// Two source LISTs are reasonable: one for the initial
	// dirPath, plus possibly the "./" entry recursing once into
	// the canonicalized same path (which then dedupes). The
	// "../" entry should be rejected by the path-escape guard
	// without ever LIST-ing. The sanity cap above catches any
	// regression that lets recursion run free.
	if n := listCalls.Load(); n > 5 {
		t.Errorf("LIST called %d times on a cyclic source — cycle/escape guard not bounding recursion tightly enough", n)
	}
}

func TestHandleMarkGraphExportEmptyStore(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkGraphExport(withAliceClaims(context.Background()), callToolReq("mark_graph_export", nil))
	if err != nil {
		t.Fatalf("handleMarkGraphExport: %v", err)
	}
	if res.IsError {
		t.Errorf("export on empty store should not be an error: %s", toolResultText(t, res))
	}
	// Empty store still produces a valid markdown export
	// (likely a header + zero rows). Just assert non-empty
	// output exists.
	if toolResultText(t, res) == "" {
		t.Error("empty-store export produced empty output")
	}
}

func TestHandleMarkGraphExportAfterCrawlIncludesCrawledNodes(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			switch path {
			case "/a.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody("a", "/b.md"),
				}}, nil
			case "/b.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   crawlBody("b"),
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	if res, err := g.handleMarkGraph(withAliceClaims(context.Background()), callToolReq("mark_graph", map[string]any{
		"url": "mark://team-a/a.md",
	})); err != nil || res.IsError {
		t.Fatalf("seed crawl: %v %v", err, res.IsError)
	}
	res, err := g.handleMarkGraphExport(withAliceClaims(context.Background()), callToolReq("mark_graph_export", nil))
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	text := toolResultText(t, res)
	for _, want := range []string{"a.md", "b.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("export missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkGraphPublishMissingArgs(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkGraphPublish(withAliceClaims(context.Background()), callToolReq("mark_graph_publish", map[string]any{
		"url": "mark://team-a/graph.md",
		// expected_version intentionally missing
	}))
	if err != nil {
		t.Fatalf("handleMarkGraphPublish: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing expected_version, want true")
	}
}

func TestHandleMarkGraphPublishForwardsThroughDispatchWithAuth(t *testing.T) {
	cfg := mcpTestConfig()
	var publishedBody string
	d := &fakeDispatcher{
		publishFn: func(_, _, body, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			publishedBody = body
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1", "modified": "2026-05-22T10:00:00Z"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkGraphPublish(withAliceClaims(context.Background()), callToolReq("mark_graph_publish", map[string]any{
		"url":              "mark://team-a/graph.md",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkGraphPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	if publishedBody == "" {
		t.Error("dispatcher saw empty body — graph publish should forward graphstore.Export() output")
	}
	text := toolResultText(t, res)
	for _, want := range []string{"Published graph", "status: ok", "version: 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
}

// indexManifestBody builds a minimal agent manifest the dispatcher
// can return when the test wants to satisfy the manifest check.
const indexManifestBody = "# Agent Manifest\n\nThis world accepts index publications.\n"

func TestHandleMarkIndexHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "hub",
		Namespace:    "hub",
		TokensSecret: "hub-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/*"},
		},
	})
	var publishCalled atomic.Bool
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			if strings.HasSuffix(path, protocol.WellKnownManifestPath) {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   indexManifestBody,
				}}, nil
			}
			if path == "/foo.md" {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Metadata: map[string]string{
						"content-hash": "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					},
					Body: "doc body",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		listFn: func(_, path, _ string) (fetch.Result, error) {
			if path == "/" {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   "- [foo](foo.md)\n",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			publishCalled.Store(true)
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1", "modified": "2026-05-22T11:00:00Z"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkIndex(withAliceClaims(context.Background()), callToolReq("mark_index", map[string]any{
		"source": "mark://team-a/",
		"target": "mark://hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkIndex: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	if !publishCalled.Load() {
		t.Error("publish was not called; expected the index doc to be written to target")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "Indexed 1 documents") {
		t.Errorf("expected indexed-count line, got:\n%s", text)
	}
}

func TestHandleMarkIndexBlocksWhenTargetHasNoManifest(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "hub",
		Namespace:    "hub",
		TokensSecret: "hub-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/*"},
		},
	})
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			// All manifest fetches return not-found.
			if strings.HasSuffix(path, protocol.WellKnownManifestPath) {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkIndex(withAliceClaims(context.Background()), callToolReq("mark_index", map[string]any{
		"source": "mark://team-a/",
		"target": "mark://hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkIndex: %v", err)
	}
	if !res.IsError {
		t.Error("expected manifest-block tool error when target has no manifest")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "no agent manifest") {
		t.Errorf("expected manifest message; got:\n%s", text)
	}
}

func TestHandleMarkIndexForceOverridesManifestBlock(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "hub",
		Namespace:    "hub",
		TokensSecret: "hub-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/*"},
		},
	})
	var publishCalled atomic.Bool
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			if strings.HasSuffix(path, protocol.WellKnownManifestPath) {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: ""}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			publishCalled.Store(true)
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkIndex(withAliceClaims(context.Background()), callToolReq("mark_index", map[string]any{
		"source": "mark://team-a/",
		"target": "mark://hub/index.md",
		"force":  true,
	}))
	if err != nil {
		t.Fatalf("handleMarkIndex: %v", err)
	}
	if res.IsError {
		t.Fatalf("force=true should have bypassed the manifest block: %s", toolResultText(t, res))
	}
	if !publishCalled.Load() {
		t.Error("force=true should still publish")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "force=true override") {
		t.Errorf("expected force-override warning; got:\n%s", text)
	}
}

func TestHandleMarkIndexDryRunReturnsBodyWithoutPublishing(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "hub",
		Namespace:    "hub",
		TokensSecret: "hub-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/*"},
		},
	})
	var publishCalled atomic.Bool
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			if strings.HasSuffix(path, protocol.WellKnownManifestPath) {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   indexManifestBody,
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: ""}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			publishCalled.Store(true)
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkIndex(withAliceClaims(context.Background()), callToolReq("mark_index", map[string]any{
		"source":  "mark://team-a/",
		"target":  "mark://hub/index.md",
		"dry_run": true,
	}))
	if err != nil {
		t.Fatalf("handleMarkIndex: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	if publishCalled.Load() {
		t.Error("dry_run should NOT publish")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "dry run") {
		t.Errorf("expected dry-run marker in output; got:\n%s", text)
	}
}

func TestHandleMarkIndexNegativeExpectedVersion(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkIndex(withAliceClaims(context.Background()), callToolReq("mark_index", map[string]any{
		"source":           "mark://team-a/",
		"target":           "mark://team-a/index.md",
		"expected_version": float64(-1),
	}))
	if err != nil {
		t.Fatalf("handleMarkIndex: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on negative expected_version, want true")
	}
}

// TestMCPGatewayMarkGraphEndToEnd drives mark_graph through the
// full Streamable HTTP transport. One end-to-end test per slice
// pins the wire-shape contract; the unit tests above cover the
// dispatch logic with finer granularity.
func TestMCPGatewayMarkGraphEndToEnd(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   crawlBody("seed", "/other.md"),
			}}, nil
		},
	}
	verifier := &fakeVerifier{claims: Claims{
		Subject: "google|alice", Email: "alice@example.com", EmailVerified: true,
	}}
	signer := newTestSigner(t)
	k8s := fake.NewSimpleClientset()
	brokerSrv := NewServer(cfg, signer, verifier, k8s, nil, nil, nil)
	brokerSrv.clock = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	ts := httptest.NewServer(brokerSrv.MCPGatewayWith("test", d))
	t.Cleanup(ts.Close)

	initR := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if initR.HTTPStatus != http.StatusOK {
		t.Fatalf("initialize: status = %d", initR.HTTPStatus)
	}
	sessionID := initR.Headers[mcpSessionHeader]
	if sessionID == "" {
		t.Fatalf("initialize response missing %s header — session negotiation regressed", mcpSessionHeader)
	}

	resp := mcpRequest(t, ts.URL, "alice-token", sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "mark_graph",
			"arguments": map[string]any{
				"url": "mark://team-a/seed.md",
			},
		},
	})
	if resp.HTTPStatus != http.StatusOK {
		t.Fatalf("tools/call: status = %d, body = %s", resp.HTTPStatus, resp.RawBody)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call JSON-RPC error: %+v", resp.Error)
	}
	isErr, _ := resp.Result["isError"].(bool)
	if isErr {
		t.Fatalf("tools/call isError = true: %+v", resp.Result)
	}
	contents, _ := resp.Result["content"].([]any)
	if len(contents) == 0 {
		t.Fatalf("content empty")
	}
	first, _ := contents[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, "Crawled") {
		t.Errorf("end-to-end graph output missing 'Crawled' header; got:\n%s", text)
	}
}

// brokerCrawlParseURL must accept URLs the strict parseToolURL
// would reject (queries, fragments) because crawled docs
// legitimately carry them. Pin the lenient behavior.
func TestBrokerCrawlParseURLLenientAboutFragmentsAndQueries(t *testing.T) {
	cases := []struct {
		raw       string
		wantWorld string
		wantPath  string
	}{
		{"mark://team-a/foo.md", "team-a", "/foo.md"},
		{"mark://team-a/foo.md#section", "team-a", "/foo.md"},
		{"mark://team-a/foo.md?rev=2", "team-a", "/foo.md"},
		{"mark://team-a", "team-a", "/"},
	}
	for _, tc := range cases {
		w, p, err := brokerCrawlParseURL(tc.raw)
		if err != nil {
			t.Errorf("%q: parse err = %v", tc.raw, err)
			continue
		}
		if w != tc.wantWorld {
			t.Errorf("%q: world = %q, want %q", tc.raw, w, tc.wantWorld)
		}
		if p != tc.wantPath {
			t.Errorf("%q: path = %q, want %q", tc.raw, p, tc.wantPath)
		}
	}
}

func TestBrokerCrawlParseURLRejectsNonMark(t *testing.T) {
	for _, raw := range []string{"https://example.com/foo", "mark:///foo"} {
		if _, _, err := brokerCrawlParseURL(raw); err == nil {
			t.Errorf("%q: expected parse error", raw)
		}
	}
}

// silenceGraphTestNoise keeps go vet happy when the test
// imports the graph package without obviously using it.
var _ = graph.New
