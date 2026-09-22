package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"path/filepath"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
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

func TestToolDefinitions(t *testing.T) {
	tests := []struct {
		name         string
		tool         mcp.Tool
		wantName     string
		wantRequired []string
		wantDesc     string // substring to check
	}{
		{
			name:         "mark_fetch with host",
			tool:         mcpfmt.FetchTool(urlDesc("mark://example.com:6309"), ""),
			wantName:     "mark_fetch",
			wantRequired: []string{"url"},
			wantDesc:     "Fetch a document",
		},
		{
			name:         "mark_backlinks",
			tool:         mcpfmt.BacklinksTool(urlDesc(""), ""),
			wantName:     "mark_backlinks",
			wantRequired: []string{"url"},
			wantDesc:     "Documents linking to a URL",
		},
		{
			name:         "mark_fetch without host",
			tool:         mcpfmt.FetchTool(urlDesc(""), ""),
			wantName:     "mark_fetch",
			wantRequired: []string{"url"},
			wantDesc:     "Fetch a document",
		},
		{
			name:         "mark_list",
			tool:         mcpfmt.ListTool(urlDesc(""), ""),
			wantName:     "mark_list",
			wantRequired: []string{"url"},
			wantDesc:     "List documents",
		},
		{
			name:         "mark_graph",
			tool:         mcpfmt.GraphTool(urlDesc(""), ""),
			wantName:     "mark_graph",
			wantRequired: []string{"url"},
			wantDesc:     "Crawl outbound links",
		},
		{
			name:         "mark_versions",
			tool:         mcpfmt.VersionsTool(urlDesc(""), ""),
			wantName:     "mark_versions",
			wantRequired: []string{"url"},
			wantDesc:     "Version history",
		},
		{
			name:         "mark_publish",
			tool:         mcpfmt.PublishTool(urlDesc(""), requiresToken),
			wantName:     "mark_publish",
			wantRequired: []string{"url", "body", "expected_version"},
			wantDesc:     "Publish or update",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.tool.Name != tt.wantName {
				t.Errorf("name = %q, want %q", tt.tool.Name, tt.wantName)
			}
			if !strings.Contains(tt.tool.Description, tt.wantDesc) {
				t.Errorf("description %q does not contain %q", tt.tool.Description, tt.wantDesc)
			}
			schema := tt.tool.InputSchema
			for _, req := range tt.wantRequired {
				if !slices.Contains(schema.Required, req) {
					t.Errorf("required params %v missing %q", schema.Required, req)
				}
				if _, ok := schema.Properties[req]; !ok {
					t.Errorf("properties missing key %q", req)
				}
			}
		})
	}
}

func TestHostInstructions(t *testing.T) {
	t.Run("with host", func(t *testing.T) {
		hint := hostInstructions("mark://example.com:6309")
		if !strings.Contains(hint, "example.com:6309") {
			t.Errorf("hint %q should contain the host", hint)
		}
		if !strings.Contains(hint, "bare paths") {
			t.Errorf("hint %q should mention bare paths", hint)
		}
	})

	t.Run("without host", func(t *testing.T) {
		hint := hostInstructions("")
		if !strings.Contains(hint, "mark://") {
			t.Errorf("hint %q should mention mark:// URLs", hint)
		}
	})
}

func TestURLDesc(t *testing.T) {
	t.Run("with host", func(t *testing.T) {
		desc := urlDesc("mark://example.com:6309")
		if !strings.Contains(desc, "path, e.g. /") {
			t.Errorf("desc %q should show a bare path example", desc)
		}
	})

	t.Run("without host", func(t *testing.T) {
		desc := urlDesc("")
		if !strings.Contains(desc, "mark://") {
			t.Errorf("desc %q should mention mark:// URL", desc)
		}
	})
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

func TestAgentName(t *testing.T) {
	t.Run("returns client name from session", func(t *testing.T) {
		s := mcpserver.NewMCPServer("test", "0.1.0")
		session := mcpserver.NewInProcessSession("test-session", nil)
		session.SetClientInfo(mcp.Implementation{Name: "claude-code", Version: "1.0"})
		ctx := s.WithContext(context.Background(), session)

		if got := agentName(ctx); got != "claude-code" {
			t.Errorf("agent = %q, want %q", got, "claude-code")
		}
	})

	t.Run("falls back to unknown without session", func(t *testing.T) {
		if got := agentName(context.Background()); got != "unknown" {
			t.Errorf("agent = %q, want %q", got, "unknown")
		}
	})

	t.Run("falls back to unknown with empty client name", func(t *testing.T) {
		s := mcpserver.NewMCPServer("test", "0.1.0")
		session := mcpserver.NewInProcessSession("test-session", nil)
		ctx := s.WithContext(context.Background(), session)

		if got := agentName(ctx); got != "unknown" {
			t.Errorf("agent = %q, want %q", got, "unknown")
		}
	})
}

func TestToolDefinition_MarkDiscover(t *testing.T) {
	t.Run("url is optional", func(t *testing.T) {
		tool := markDiscoverTool()
		if tool.Name != "mark_discover" {
			t.Errorf("name = %q, want mark_discover", tool.Name)
		}
		if slices.Contains(tool.InputSchema.Required, "url") {
			t.Error("url should not be required")
		}
		if !strings.Contains(tool.Description, "agent manifest") {
			t.Error("description should mention agent manifest")
		}
	})
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

func TestToolDefinition_MarkAppend(t *testing.T) {
	tool := mcpfmt.AppendTool(urlDesc("mark://example.com:6309"), requiresToken)
	if tool.Name != "mark_append" {
		t.Errorf("name = %q, want mark_append", tool.Name)
	}
	if slices.Contains(tool.InputSchema.Required, "expected_version") {
		t.Error("expected_version should not be required")
	}
	if !strings.Contains(tool.Description, "optional") {
		t.Error("description should mention expected_version is optional")
	}
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

// --- mark_resolve tests ---

func TestHandlerMarkResolve_Success(t *testing.T) {
	hash := "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	indexBody := "| Hash | Server | Path |\n|------|--------|------|\n| " + hash + " | mark://docs.example.com | /guide.md |\n"

	sc := &stubClient{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			path := r.Path
			if path == "/index.md" {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   indexBody,
				}}, nil
			}
			if path == "/"+hash {
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"content-hash": hash, "version": "1"},
					Body:     "# Guide",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
		},
	}

	h := &handler{client: sc}
	result, err := h.markResolve(context.Background(), newCallToolRequest(map[string]any{
		"hash":  hash,
		"index": "mark://hub.example.com/index.md",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "# Guide") {
		t.Errorf("result should contain document body, got: %s", text)
	}
}

// --- mark_index tests ---

func TestHandlerMarkIndex_Success(t *testing.T) {
	hash := "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	var publishedBodies []string
	var publishedPaths []string

	sc := &stubClient{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if r.Path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   "# Agent Manifest\nThis is a hub.",
				}}, nil
			}
			if r.Host == "hub.com:6309" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": hash},
				Body:     "# Doc",
			}}, nil
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			if r.Path == "/" {
				return fetchtest.ListPage("/", "", "doc.md"), nil
			}
			return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
		},
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			path, body, expectedVersion := r.Path, r.Body, r.ExpectedVersion
			publishedBodies = append(publishedBodies, body)
			publishedPaths = append(publishedPaths, path)
			if expectedVersion != 0 {
				t.Errorf("publish %s expected_version = %d, want 0", path, expectedVersion)
			}
			return fetch.Result{Response: protocol.Response{
				Status:   "created",
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}

	h := &handler{client: sc, defaultHost: "mark://hub.com", token: "test-token"}
	result, err := h.markIndex(context.Background(), newCallToolRequest(map[string]any{
		"source":           "mark://source.com",
		"target":           "mark://hub.com/indexes/source.md",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	published := strings.Join(publishedBodies, "\n")
	if !strings.Contains(published, hash) {
		t.Error("published index should contain the content hash")
	}
	if !strings.Contains(published, "/doc.md") {
		t.Error("published index should contain the document path")
	}
	if len(publishedPaths) != 2 || publishedPaths[1] != "/indexes/source.md" {
		t.Errorf("publish paths = %v, want shard then manifest", publishedPaths)
	}
	// Servers are named by identity, never by dial address (ADR 0005, ADR 0018).
	text := result.Content[0].(mcp.TextContent).Text
	for _, want := range []string{"| mark://source.com |", "> Source: mark://source.com\n"} {
		if !strings.Contains(published, want) {
			t.Errorf("published index missing %q:\n%s", want, published)
		}
	}
	if !strings.Contains(text, "from mark://source.com\n") {
		t.Errorf("tool output should name the source by identity:\n%s", text)
	}
	if strings.Contains(published+text, ":6309") {
		t.Errorf("dial port leaked into index or output:\n%s\n%s", published, text)
	}
}

// --- Tool definition tests for new tools ---

func TestToolDefinition_MarkResolve(t *testing.T) {
	tool := markResolveTool("")
	if tool.Name != "mark_resolve" {
		t.Errorf("name = %q, want mark_resolve", tool.Name)
	}
	if !slices.Contains(tool.InputSchema.Required, "hash") {
		t.Error("hash should be required")
	}
	if !slices.Contains(tool.InputSchema.Required, "index") {
		t.Error("index should be required")
	}
}

func TestToolDefinition_MarkIndex(t *testing.T) {
	tool := markIndexTool("")
	if tool.Name != "mark_index" {
		t.Errorf("name = %q, want mark_index", tool.Name)
	}
	if !slices.Contains(tool.InputSchema.Required, "source") {
		t.Error("source should be required")
	}
	if !slices.Contains(tool.InputSchema.Required, "target") {
		t.Error("target should be required")
	}
	if slices.Contains(tool.InputSchema.Required, "expected_version") {
		t.Error("expected_version should not be required")
	}
	if slices.Contains(tool.InputSchema.Required, "dry_run") {
		t.Error("dry_run should not be required")
	}
	if slices.Contains(tool.InputSchema.Required, "force") {
		t.Error("force should not be required")
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

func TestToolDefinition_MarkGraphPublish(t *testing.T) {
	tool := mcpfmt.GraphPublishTool("graph document target, e.g. "+urlDesc(""), requiresToken)
	if tool.Name != "mark_graph_publish" {
		t.Errorf("name = %q, want mark_graph_publish", tool.Name)
	}
	if !slices.Contains(tool.InputSchema.Required, "url") {
		t.Error("url should be required")
	}
	if !slices.Contains(tool.InputSchema.Required, "expected_version") {
		t.Error("expected_version should be required")
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
