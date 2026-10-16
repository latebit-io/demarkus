package main

import (
	"context"
	"strings"
	"testing"

	"path/filepath"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name        string
		defaultHost string
		rawURL      string
		wantHost    string
		wantPath    string
		wantErr     bool
	}{
		{
			name:        "bare path with default host",
			defaultHost: "mark://localhost:6309",
			rawURL:      "/index.md",
			wantHost:    "localhost:6309",
			wantPath:    "/index.md",
		},
		{
			name:        "bare path with default host no port",
			defaultHost: "mark://example.com",
			rawURL:      "/docs/guide.md",
			wantHost:    "example.com:6309",
			wantPath:    "/docs/guide.md",
		},
		{
			name:        "full mark URL ignores default host",
			defaultHost: "mark://localhost:6309",
			rawURL:      "mark://other:6309/page.md",
			wantHost:    "other:6309",
			wantPath:    "/page.md",
		},
		{
			name:     "full mark URL without default host",
			rawURL:   "mark://example.com:6309/index.md",
			wantHost: "example.com:6309",
			wantPath: "/index.md",
		},
		{
			name:    "bare path without default host errors",
			rawURL:  "/index.md",
			wantErr: true,
		},
		{
			name:    "invalid scheme errors",
			rawURL:  "http://example.com/index.md",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &handler{defaultHost: tt.defaultHost}
			target, err := h.resolveURL(tt.rawURL)
			host, path := target.DialHost(), target.Path
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

// newCallToolRequest builds a CallToolRequest with the given arguments.
func newCallToolRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: args,
		},
	}
}

func TestHandlerMarkFetch_InvalidURL(t *testing.T) {
	h := &handler{client: &stubClient{}} // no default host: a bare path cannot resolve
	ctx := context.Background()

	result, err := h.markFetch(ctx, newCallToolRequest(map[string]any{"url": "/bare-path"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestHandlerMarkListForwardsPagination(t *testing.T) {
	var got fetch.ListRequest
	h := &handler{client: &stubClient{
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			got = r
			return fetchtest.ListPage("/", ""), nil
		},
	}}
	result, err := h.markList(t.Context(), newCallToolRequest(map[string]any{
		"url":              "mark://example.com/",
		"include_archived": true,
		"cursor":           "next",
		"page_size":        25,
	}))
	if err != nil || result.IsError {
		t.Fatalf("markList = (%+v, %v)", result, err)
	}
	// The token comes from the machine's own store, so it is not pinned here.
	want := fetch.ListRequest{Host: "example.com:6309", Path: "/", Token: got.Token, IncludeArchived: true, Cursor: "next", PageSize: 25}
	if got != want {
		t.Errorf("request = %+v, want %+v", got, want)
	}
	result, err = h.markList(t.Context(), newCallToolRequest(map[string]any{
		"url":       "mark://example.com/",
		"page_size": 1.5,
	}))
	if err != nil || !result.IsError {
		t.Fatalf("fractional page_size = (%+v, %v), want tool error", result, err)
	}
}

func TestHandlerMarkPublish_NoToken(t *testing.T) {
	h := &handler{client: &stubClient{}} // no token
	ctx := context.Background()

	result, err := h.markPublish(ctx, newCallToolRequest(map[string]any{
		"url":              "mark://example.com/doc.md",
		"body":             "# Hello",
		"expected_version": float64(0), // arguments are checked before authorization
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires a token")
}

func TestHandlerMarkDiscover_NoHostNoURL(t *testing.T) {
	h := &handler{client: &stubClient{}} // no default host
	ctx := context.Background()

	result, err := h.markDiscover(ctx, newCallToolRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "no server specified")
}

func TestHandlerMarkDiscover_InvalidURL(t *testing.T) {
	h := &handler{client: &stubClient{}}
	ctx := context.Background()

	result, err := h.markDiscover(ctx, newCallToolRequest(map[string]any{"url": "/bare-path"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestHandlerMarkAppend_NoToken(t *testing.T) {
	h := &handler{client: &stubClient{}}
	ctx := context.Background()

	result, err := h.markAppend(ctx, newCallToolRequest(map[string]any{
		"url":  "mark://example.com/doc.md",
		"body": "appended content",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires a token")
}

// stubClient is the shared scriptable client; see client/fetchtest.
type stubClient = fetchtest.Client

func TestHandlerMarkPublish_Metadata(t *testing.T) {
	var gotMeta map[string]string
	sc := &stubClient{
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			gotMeta = r.Metadata
			return fetch.Result{Response: protocol.Response{
				Status:   "created",
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://example.com", token: "test-token"}

	res, err := h.markPublish(context.Background(), newCallToolRequest(map[string]any{
		"url":              "mark://example.com/doc.md",
		"body":             "# Doc",
		"expected_version": float64(0),
		"on_conflict":      "fail",
		"metadata":         map[string]any{"tags": "go,auth", "importance": 0.9},
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}
	if gotMeta["tags"] != "go,auth" {
		t.Errorf("tags = %q, want go,auth", gotMeta["tags"])
	}
	if gotMeta["importance"] != "0.9" {
		t.Errorf("importance = %q, want 0.9", gotMeta["importance"])
	}
	// Agent identity is still set (applied last so callers can't spoof it).
	if gotMeta["agent"] == "" {
		t.Errorf("agent identity missing from publisher metadata: %v", gotMeta)
	}
}

func TestHandlerMarkLookup(t *testing.T) {
	var gotScope, gotQuery string
	var gotOpts fetch.LookupRequest
	sc := &stubClient{
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			gotScope, gotQuery, gotOpts = r.Scope, r.Query, r
			return fetchtest.Golden(t, "lookup"), nil
		},
	}

	h := &handler{client: sc, defaultHost: "mark://example.com", token: "test-token"}
	result, err := h.markLookup(context.Background(), newCallToolRequest(map[string]any{
		"url":    "mark://example.com/docs/",
		"query":  "auth middleware",
		"filter": "project=broker",
		"limit":  float64(5),
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	if gotScope != "/docs/" {
		t.Errorf("scope = %q, want /docs/", gotScope)
	}
	if gotQuery != "auth middleware" {
		t.Errorf("query = %q, want %q", gotQuery, "auth middleware")
	}
	if gotOpts.Filter != "project=broker" || gotOpts.Limit != 5 {
		t.Errorf("opts = %+v, want {Filter:project=broker Limit:5}", gotOpts)
	}
}

func TestHandlerMarkAppend_ExplicitVersion(t *testing.T) {
	var capturedVersion int
	sc := &stubClient{
		AppendFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			capturedVersion = r.ExpectedVersion
			return fetch.Result{
				Response: protocol.Response{
					Status:   "created",
					Metadata: map[string]string{"version": "4", "modified": "2026-03-07T00:00:00Z"},
				},
			}, nil
		},
	}

	h := &handler{client: sc, defaultHost: "mark://example.com", token: "test-token"}
	ctx := context.Background()

	result, err := h.markAppend(ctx, newCallToolRequest(map[string]any{
		"url":              "mark://example.com/doc.md",
		"body":             "appended content",
		"expected_version": float64(3),
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	if capturedVersion != 3 {
		t.Errorf("expected_version passed to Append = %d, want 3", capturedVersion)
	}
}

func TestHandlerMarkBacklinks_BarePathWithoutHost(t *testing.T) {
	h := &handler{client: &stubClient{}}
	ctx := context.Background()

	result, err := h.markBacklinks(ctx, newCallToolRequest(map[string]any{"url": "/some/path.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestHandlerMarkBacklinks_HappyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Page A", Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://host:6309/b.md", Title: "Page B", Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://host:6309/c.md", Title: "Page C", Status: "ok"})
	g.AddEdge("mark://host:6309/a.md", "mark://host:6309/c.md")
	g.AddEdge("mark://host:6309/b.md", "mark://host:6309/c.md")
	gs.Merge(g, nil)

	// The freshness pass re-reads each source, so the sources still hold their links.
	sources := &stubClient{FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		title := map[string]string{"/a.md": "Page A", "/b.md": "Page B"}[r.Path]
		if title == "" {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# " + title + "\n\n[Page C](/c.md)\n"}}, nil
	}}
	h := &handler{client: sources, graphStore: gs}
	ctx := context.Background()

	result, err := h.markBacklinks(ctx, newCallToolRequest(map[string]any{"url": "mark://host:6309/c.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success result")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "Page A") {
		t.Errorf("expected backlink title 'Page A' in output: %s", text.Text)
	}
	if !strings.Contains(text.Text, "Page B") {
		t.Errorf("expected backlink title 'Page B' in output: %s", text.Text)
	}
	if !strings.Contains(text.Text, "mark://host/a.md") {
		t.Errorf("expected backlink URL in output: %s", text.Text)
	}
}

func TestHandlerMarkBacklinks_NilStore(t *testing.T) {
	h := &handler{client: &stubClient{}, defaultHost: "mark://host:6309"}
	ctx := context.Background()

	result, err := h.markBacklinks(ctx, newCallToolRequest(map[string]any{"url": "mark://host:6309/c.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "graph store not available")
}

func TestHandlerMarkGraphExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Page A", Status: "ok", LinkCount: 2})
	g.AddNode(&graph.Node{URL: "mark://host:6309/b.md", Title: "Page B", Status: "ok", LinkCount: 1})
	g.AddEdge("mark://host:6309/a.md", "mark://host:6309/b.md")
	gs.Merge(g, nil)

	h := &handler{client: &stubClient{}, graphStore: gs}
	ctx := context.Background()

	result, err := h.markGraphExport(ctx, newCallToolRequest(nil))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success result")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "# Document Graph") {
		t.Error("expected markdown title in output")
	}
	if !strings.Contains(text.Text, "mark://host/a.md") {
		t.Error("expected node URL in output")
	}
	if !strings.Contains(text.Text, "## Edges") {
		t.Error("expected edges section in output")
	}
}

func TestHandlerMarkGraphPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Page A", Status: "ok", LinkCount: 2})
	g.AddNode(&graph.Node{URL: "mark://host:6309/b.md", Title: "Page B", Status: "ok", LinkCount: 1})
	g.AddEdge("mark://host:6309/a.md", "mark://host:6309/b.md")
	gs.Merge(g, nil)

	var publishedBody string
	sc := &stubClient{
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			publishedBody = r.Body
			return fetch.Result{Response: protocol.Response{
				Status:   "created",
				Metadata: map[string]string{"version": "1", "modified": "2026-03-08T12:00:00Z"},
			}}, nil
		},
	}

	h := &handler{client: sc, graphStore: gs, defaultHost: "mark://target.com", token: "test-token"}
	ctx := context.Background()

	result, err := h.markGraphPublish(ctx, newCallToolRequest(map[string]any{
		"url":              "mark://target.com/graph.md",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}

	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Published graph") {
		t.Error("expected 'Published graph' in output")
	}
	if !strings.Contains(text, "2 nodes") {
		t.Errorf("expected node count in output: %s", text)
	}
	if !strings.Contains(publishedBody, "# Document Graph") {
		t.Error("published body should contain graph export")
	}
	if !strings.Contains(publishedBody, "mark://host/a.md") {
		t.Error("published body should contain node URLs")
	}
}

func TestHandlerMarkGraphPublish_NoToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	h := &handler{client: &stubClient{}, graphStore: gs}
	ctx := context.Background()

	result, err := h.markGraphPublish(ctx, newCallToolRequest(map[string]any{
		"url":              "mark://target.com/graph.md",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires a token")
}

// assertIsToolError checks that a CallToolResult is an error containing the given substring.
func assertIsToolError(t *testing.T, result *mcp.CallToolResult, substr string) {
	t.Helper()
	if !result.IsError {
		t.Fatal("expected tool error result")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content in error result")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, substr) {
		t.Errorf("error text %q does not contain %q", text.Text, substr)
	}
}

func TestResolveToken_ScopedToDefaultHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DEMARKUS_AUTH", "env-token")
	tests := []struct {
		name        string
		defaultHost string
		flagToken   string
		host        string
		want        string
	}{
		{"flag on default host", "mark://example.com", "flag-token", "example.com:6309", "flag-token"},
		{"env on default host", "mark://example.com", "", "example.com:6309", "env-token"},
		{"foreign host gets nothing", "mark://example.com", "flag-token", "evil.example:6309", ""},
		{"no default host scopes to no host", "", "flag-token", "example.com:6309", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &handler{defaultHost: tt.defaultHost, token: tt.flagToken}
			if got := h.resolveToken(tt.host); got != tt.want {
				t.Errorf("resolveToken(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

// The tool names the failure once; the parser only says what is wrong.
func TestHandlerMarkVersions_InvalidURLSaysSoOnce(t *testing.T) {
	h := &handler{client: &stubClient{}}
	for url, want := range map[string]string{
		"http://example.com/doc.md": "invalid URL: unsupported scheme: http (expected mark://)",
		"mark://host:0/doc.md":      `invalid URL: port "0" must be between 1 and 65535`,
		"mark:///doc.md":            "invalid URL: authority host is required",
	} {
		result, err := h.markVersions(t.Context(), newCallToolRequest(map[string]any{"url": url}))
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if got := resultText(t, result); !result.IsError || got != want {
			t.Errorf("%s: got %q, want %q", url, got, want)
		}
	}
}

// The tool bodies are tested in client/marktools. These prove the four
// handlers with no other happy path here parse their arguments and reach one.
func TestHandlersReachTheirBodies(t *testing.T) {
	t.Run("versions", func(t *testing.T) {
		sc := &stubClient{VersionsFn: func(_ context.Context, _ fetch.VersionsRequest) (fetch.Result, error) {
			return fetchtest.Versions("/doc.md", 3), nil
		}}
		h := &handler{client: sc}
		result, err := h.markVersions(t.Context(), newCallToolRequest(map[string]any{"url": "mark://example.com/doc.md"}))
		if err != nil || result.IsError {
			t.Fatalf("markVersions = (%+v, %v)", result, err)
		}
		if got := resultText(t, result); !strings.Contains(got, "current: 3") {
			t.Errorf("versions text = %q", got)
		}
		if len(sc.VersionsCalls) != 1 || sc.VersionsCalls[0].Host != "example.com:6309" || sc.VersionsCalls[0].Path != "/doc.md" {
			t.Errorf("calls = %+v", sc.VersionsCalls)
		}
	})

	t.Run("archive", func(t *testing.T) {
		sc := &stubClient{}
		h := &handler{client: sc, defaultHost: "mark://example.com", token: "test-token"}
		result, err := h.markArchive(t.Context(), newCallToolRequest(map[string]any{"url": "/doc.md"}))
		if err != nil || result.IsError {
			t.Fatalf("markArchive = (%+v, %v)", result, err)
		}
		want := fetch.ArchiveRequest{Host: "example.com:6309", Path: "/doc.md", Token: "test-token"}
		if len(sc.ArchiveCalls) != 1 || sc.ArchiveCalls[0] != want {
			t.Errorf("calls = %+v, want %+v", sc.ArchiveCalls, want)
		}
		if got := resultText(t, result); !strings.Contains(got, "archived: true") {
			t.Errorf("archive text = %q", got)
		}
	})

	t.Run("discover", func(t *testing.T) {
		sc := &stubClient{FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Agent Manifest\n"}}, nil
		}}
		h := &handler{client: sc}
		result, err := h.markDiscover(t.Context(), newCallToolRequest(map[string]any{"url": "mark://example.com/any/doc.md"}))
		if err != nil || result.IsError {
			t.Fatalf("markDiscover = (%+v, %v)", result, err)
		}
		if len(sc.FetchCalls) != 1 || sc.FetchCalls[0].Host != "example.com:6309" || sc.FetchCalls[0].Path != protocol.WellKnownManifestPath {
			t.Errorf("calls = %+v, want the server's manifest", sc.FetchCalls)
		}
		if got := resultText(t, result); !strings.Contains(got, "# Agent Manifest") {
			t.Errorf("discover text = %q", got)
		}
	})

	t.Run("graph", func(t *testing.T) {
		gs, err := graphstore.Load(filepath.Join(t.TempDir(), "graph.json"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		sc := &stubClient{FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Index\n"}}, nil
		}}
		h := &handler{client: sc, graphStore: gs}
		result, err := h.markGraph(t.Context(), newCallToolRequest(map[string]any{"url": "mark://host:6309/index.md", "depth": float64(1)}))
		if err != nil || result.IsError {
			t.Fatalf("markGraph = (%+v, %v)", result, err)
		}
		// Crawled and kept under the identity, which omits the default port (ADR 0005).
		if got := resultText(t, result); !strings.Contains(got, "mark://host/index.md") || gs.GetNode("mark://host/index.md") == nil {
			t.Errorf("graph text = %q, node stored = %v", got, gs.GetNode("mark://host/index.md") != nil)
		}
	})
}
