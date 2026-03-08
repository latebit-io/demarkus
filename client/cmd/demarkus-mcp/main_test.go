package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"path/filepath"

	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/latebit/demarkus/client/internal/graphstore"
	"github.com/latebit/demarkus/protocol"
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
			host, path, err := h.resolveURL(tt.rawURL)
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
			tool:         markFetchTool("mark://example.com:6309"),
			wantName:     "mark_fetch",
			wantRequired: []string{"url"},
			wantDesc:     "Fetch a document",
		},
		{
			name:         "mark_fetch without host",
			tool:         markFetchTool(""),
			wantName:     "mark_fetch",
			wantRequired: []string{"url"},
			wantDesc:     "mark://",
		},
		{
			name:         "mark_list",
			tool:         markListTool(""),
			wantName:     "mark_list",
			wantRequired: []string{"url"},
			wantDesc:     "List documents",
		},
		{
			name:         "mark_graph",
			tool:         markGraphTool(""),
			wantName:     "mark_graph",
			wantRequired: []string{"url"},
			wantDesc:     "Crawl outbound links",
		},
		{
			name:         "mark_versions",
			tool:         markVersionsTool(""),
			wantName:     "mark_versions",
			wantRequired: []string{"url"},
			wantDesc:     "version history",
		},
		{
			name:         "mark_publish",
			tool:         markPublishTool(""),
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

func TestURLHint(t *testing.T) {
	t.Run("with host", func(t *testing.T) {
		hint := urlHint("mark://example.com:6309")
		if !strings.Contains(hint, "example.com:6309") {
			t.Errorf("hint %q should contain the host", hint)
		}
		if !strings.Contains(hint, "bare paths") {
			t.Errorf("hint %q should mention bare paths", hint)
		}
	})

	t.Run("without host", func(t *testing.T) {
		hint := urlHint("")
		if !strings.Contains(hint, "mark://") {
			t.Errorf("hint %q should mention mark:// URLs", hint)
		}
	})
}

func TestURLDesc(t *testing.T) {
	t.Run("with host", func(t *testing.T) {
		desc := urlDesc("mark://example.com:6309")
		if !strings.Contains(desc, "bare path") {
			t.Errorf("desc %q should mention bare path", desc)
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
	h := &handler{} // no default host, no client needed for URL validation
	ctx := context.Background()

	result, err := h.markFetch(ctx, newCallToolRequest(map[string]any{"url": "/bare-path"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestHandlerMarkList_InvalidURL(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markList(ctx, newCallToolRequest(map[string]any{"url": "/bare-path"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestHandlerMarkPublish_NoToken(t *testing.T) {
	h := &handler{} // no token
	ctx := context.Background()

	result, err := h.markPublish(ctx, newCallToolRequest(map[string]any{
		"url":  "mark://example.com/doc.md",
		"body": "# Hello",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires a token")
}

func TestHandlerMarkFetch_MissingURL(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markFetch(ctx, newCallToolRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "url is required")
}

func TestHandlerMarkVersions_InvalidURL(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markVersions(ctx, newCallToolRequest(map[string]any{"url": "/bare-path"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestHandlerMarkVersions_MissingURL(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markVersions(ctx, newCallToolRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "url is required")
}

func TestHandlerMarkGraph_InvalidURL(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markGraph(ctx, newCallToolRequest(map[string]any{"url": "/bare-path"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestFormatResult(t *testing.T) {
	tests := []struct {
		name     string
		result   fetch.Result
		keys     []string
		wantSubs []string
	}{
		{
			name: "status and metadata keys present",
			result: fetch.Result{
				Response: protocol.Response{
					Status:   "ok",
					Metadata: map[string]string{"version": "3", "modified": "2026-02-23T10:00:00Z", "etag": "abc123"},
					Body:     "# Hello",
				},
			},
			keys:     []string{"version", "modified", "etag"},
			wantSubs: []string{"status: ok", "version: 3", "modified: 2026-02-23T10:00:00Z", "etag: abc123", "# Hello"},
		},
		{
			name: "missing metadata keys are skipped",
			result: fetch.Result{
				Response: protocol.Response{
					Status:   "ok",
					Metadata: map[string]string{"version": "1"},
					Body:     "content",
				},
			},
			keys:     []string{"version", "etag"},
			wantSubs: []string{"status: ok", "version: 1", "content"},
		},
		{
			name: "no body omits trailing newline",
			result: fetch.Result{
				Response: protocol.Response{
					Status:   "created",
					Metadata: map[string]string{"version": "5"},
				},
			},
			keys:     []string{"version"},
			wantSubs: []string{"status: created", "version: 5"},
		},
		{
			name: "publisher metadata included after explicit keys",
			result: fetch.Result{
				Response: protocol.Response{
					Status:   "ok",
					Metadata: map[string]string{"version": "2", "type": "journal", "author": "claude"},
					Body:     "hello",
				},
			},
			keys:     []string{"version"},
			wantSubs: []string{"status: ok", "version: 2", "type: journal", "author: claude", "hello"},
		},
		{
			name: "publisher metadata only when no explicit keys requested",
			result: fetch.Result{
				Response: protocol.Response{
					Status:   "ok",
					Metadata: map[string]string{"type": "note"},
					Body:     "body",
				},
			},
			keys:     nil,
			wantSubs: []string{"status: ok", "type: note", "body"},
		},
		{
			name: "no duplicate when publisher key also in explicit keys",
			result: fetch.Result{
				Response: protocol.Response{
					Status:   "ok",
					Metadata: map[string]string{"version": "1", "type": "log"},
					Body:     "data",
				},
			},
			keys:     []string{"version", "type"},
			wantSubs: []string{"version: 1", "type: log"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatResult(tt.result, tt.keys...)
			for _, sub := range tt.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("output %q does not contain %q", got, sub)
				}
			}
			// Verify no metadata key appears more than once.
			for k := range tt.result.Response.Metadata {
				prefix := k + ": "
				if count := strings.Count(got, prefix); count > 1 {
					t.Errorf("key %q appears %d times in output %q", k, count, got)
				}
			}
		})
	}
}

func TestAgentMeta(t *testing.T) {
	t.Run("returns client name from session", func(t *testing.T) {
		s := mcpserver.NewMCPServer("test", "0.1.0")
		session := mcpserver.NewInProcessSession("test-session", nil)
		session.SetClientInfo(mcp.Implementation{Name: "claude-code", Version: "1.0"})
		ctx := s.WithContext(context.Background(), session)

		meta := agentMeta(ctx)
		if meta["agent"] != "claude-code" {
			t.Errorf("agent = %q, want %q", meta["agent"], "claude-code")
		}
	})

	t.Run("falls back to unknown without session", func(t *testing.T) {
		meta := agentMeta(context.Background())
		if meta["agent"] != "unknown" {
			t.Errorf("agent = %q, want %q", meta["agent"], "unknown")
		}
	})

	t.Run("falls back to unknown with empty client name", func(t *testing.T) {
		s := mcpserver.NewMCPServer("test", "0.1.0")
		session := mcpserver.NewInProcessSession("test-session", nil)
		ctx := s.WithContext(context.Background(), session)

		meta := agentMeta(ctx)
		if meta["agent"] != "unknown" {
			t.Errorf("agent = %q, want %q", meta["agent"], "unknown")
		}
	})
}

func TestToolDefinition_MarkDiscover(t *testing.T) {
	t.Run("url is optional", func(t *testing.T) {
		tool := markDiscoverTool("mark://example.com:6309")
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
	h := &handler{} // no default host
	ctx := context.Background()

	result, err := h.markDiscover(ctx, newCallToolRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "no server specified")
}

func TestHandlerMarkDiscover_InvalidURL(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markDiscover(ctx, newCallToolRequest(map[string]any{"url": "/bare-path"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

func TestToolDefinition_MarkAppend(t *testing.T) {
	tool := markAppendTool("mark://example.com:6309")
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
	h := &handler{}
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

// stubClient is a mock markClient for testing handler logic.
type stubClient struct {
	fetchFn    func(host, path string) (fetch.Result, error)
	listFn     func(host, path string) (fetch.Result, error)
	versionsFn func(host, path string) (fetch.Result, error)
	publishFn  func(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	appendFn   func(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
}

func (s *stubClient) Fetch(host, path string) (fetch.Result, error) {
	if s.fetchFn != nil {
		return s.fetchFn(host, path)
	}
	return fetch.Result{}, nil
}
func (s *stubClient) List(host, path string) (fetch.Result, error) {
	if s.listFn != nil {
		return s.listFn(host, path)
	}
	return fetch.Result{}, nil
}
func (s *stubClient) Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	if s.publishFn != nil {
		return s.publishFn(host, path, body, token, expectedVersion, meta)
	}
	return fetch.Result{}, nil
}
func (s *stubClient) Archive(_, _, _ string) (fetch.Result, error) {
	return fetch.Result{}, nil
}
func (s *stubClient) Versions(host, path string) (fetch.Result, error) {
	if s.versionsFn != nil {
		return s.versionsFn(host, path)
	}
	return fetch.Result{}, nil
}
func (s *stubClient) Append(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	if s.appendFn != nil {
		return s.appendFn(host, path, body, token, expectedVersion, meta)
	}
	return fetch.Result{}, nil
}

func TestHandlerMarkAppend_AutoResolveVersion(t *testing.T) {
	var capturedVersion int
	sc := &stubClient{
		versionsFn: func(_, _ string) (fetch.Result, error) {
			return fetch.Result{
				Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"current": "7", "total": "7"},
				},
			}, nil
		},
		appendFn: func(_, _, _, _ string, expectedVersion int, _ map[string]string) (fetch.Result, error) {
			capturedVersion = expectedVersion
			return fetch.Result{
				Response: protocol.Response{
					Status:   "created",
					Metadata: map[string]string{"version": "8", "modified": "2026-03-07T00:00:00Z"},
				},
			}, nil
		},
	}

	h := &handler{client: sc, token: "test-token"}
	ctx := context.Background()

	result, err := h.markAppend(ctx, newCallToolRequest(map[string]any{
		"url":  "mark://example.com/journal.md",
		"body": "new entry",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	if capturedVersion != 7 {
		t.Errorf("expected_version passed to Append = %d, want 7", capturedVersion)
	}
}

func TestHandlerMarkAppend_AutoResolveVersionNotFound(t *testing.T) {
	sc := &stubClient{
		versionsFn: func(_, _ string) (fetch.Result, error) {
			return fetch.Result{
				Response: protocol.Response{
					Status:   "not-found",
					Metadata: map[string]string{},
				},
			}, nil
		},
	}

	h := &handler{client: sc, token: "test-token"}
	ctx := context.Background()

	result, err := h.markAppend(ctx, newCallToolRequest(map[string]any{
		"url":  "mark://example.com/missing.md",
		"body": "new entry",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "not-found")
}

func TestHandlerMarkAppend_ExplicitVersion(t *testing.T) {
	var capturedVersion int
	sc := &stubClient{
		appendFn: func(_, _, _, _ string, expectedVersion int, _ map[string]string) (fetch.Result, error) {
			capturedVersion = expectedVersion
			return fetch.Result{
				Response: protocol.Response{
					Status:   "created",
					Metadata: map[string]string{"version": "4", "modified": "2026-03-07T00:00:00Z"},
				},
			}, nil
		},
	}

	h := &handler{client: sc, token: "test-token"}
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

func TestHandlerMarkAppend_NegativeVersion(t *testing.T) {
	h := &handler{token: "test-token"}
	ctx := context.Background()

	result, err := h.markAppend(ctx, newCallToolRequest(map[string]any{
		"url":              "mark://example.com/doc.md",
		"body":             "appended content",
		"expected_version": float64(-1),
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "expected_version must be >= 0")
}

// --- mark_resolve tests ---

func TestHandlerMarkResolve_Success(t *testing.T) {
	hash := "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	indexBody := "| Hash | Server | Path |\n|------|--------|------|\n| " + hash + " | mark://docs.example.com | /guide.md |\n"

	sc := &stubClient{
		fetchFn: func(_, path string) (fetch.Result, error) {
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

func TestHandlerMarkResolve_Fallback(t *testing.T) {
	hash := "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	indexBody := "| Hash | Server | Path |\n|------|--------|------|\n" +
		"| " + hash + " | mark://server1.com | /a.md |\n" +
		"| " + hash + " | mark://server2.com | /b.md |\n"

	sc := &stubClient{
		fetchFn: func(host, path string) (fetch.Result, error) {
			if strings.Contains(path, "index") {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: indexBody}}, nil
			}
			// First server fails, second succeeds.
			if host == "server1.com:6309" {
				return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": hash},
				Body:     "# Found",
			}}, nil
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
	if !strings.Contains(text, "# Found") {
		t.Errorf("should contain document from second server, got: %s", text)
	}
}

func TestHandlerMarkResolve_NotFound(t *testing.T) {
	hash := "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	indexBody := "| Hash | Server | Path |\n|------|--------|------|\n| " + hash + " | mark://server.com | /a.md |\n"

	sc := &stubClient{
		fetchFn: func(_, path string) (fetch.Result, error) {
			if strings.Contains(path, "index") {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: indexBody}}, nil
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
	assertIsToolError(t, result, "could not resolve hash")
}

func TestHandlerMarkResolve_InvalidHash(t *testing.T) {
	h := &handler{}
	result, err := h.markResolve(context.Background(), newCallToolRequest(map[string]any{
		"hash":  "not-a-hash",
		"index": "mark://hub.example.com/index.md",
	}))
	if err != nil {
		t.Fatal(err)
	}
	assertIsToolError(t, result, "invalid hash format")
}

// --- mark_index tests ---

func TestHandlerMarkIndex_Success(t *testing.T) {
	hash := "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	var publishedBody string

	sc := &stubClient{
		fetchFn: func(_, path string) (fetch.Result, error) {
			if path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   "# Agent Manifest\nThis is a hub.",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": hash},
				Body:     "# Doc",
			}}, nil
		},
		listFn: func(_, path string) (fetch.Result, error) {
			if path == "/" {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   "# Index of /\n\n- [doc.md](doc.md)\n",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
		},
		publishFn: func(_, _, body, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			publishedBody = body
			return fetch.Result{Response: protocol.Response{
				Status:   "created",
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}

	h := &handler{client: sc, token: "test-token"}
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
	if !strings.Contains(publishedBody, hash) {
		t.Error("published index should contain the content hash")
	}
	if !strings.Contains(publishedBody, "/doc.md") {
		t.Error("published index should contain the document path")
	}
}

func TestHandlerMarkIndex_DryRun(t *testing.T) {
	hash := "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sc := &stubClient{
		fetchFn: func(_, path string) (fetch.Result, error) {
			if path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Manifest"}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": hash},
				Body:     "content",
			}}, nil
		},
		listFn: func(_, path string) (fetch.Result, error) {
			if path == "/" {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   "- [a.md](a.md)\n",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			t.Fatal("publish should not be called in dry_run mode")
			return fetch.Result{}, nil
		},
	}

	h := &handler{client: sc}
	result, err := h.markIndex(context.Background(), newCallToolRequest(map[string]any{
		"source":  "mark://source.com",
		"target":  "mark://hub.com/indexes/source.md",
		"dry_run": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "dry run") {
		t.Error("dry run output should mention dry run")
	}
	if !strings.Contains(text, hash) {
		t.Error("dry run output should contain the index content")
	}
}

func TestHandlerMarkIndex_BlocksWithoutManifest(t *testing.T) {
	sc := &stubClient{
		fetchFn: func(_, path string) (fetch.Result, error) {
			if path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
		},
		listFn: func(_, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "- [a.md](a.md)\n"}}, nil
		},
	}

	h := &handler{client: sc, token: "test-token"}
	result, err := h.markIndex(context.Background(), newCallToolRequest(map[string]any{
		"source":           "mark://source.com",
		"target":           "mark://hub.com/indexes/source.md",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatal(err)
	}
	assertIsToolError(t, result, "no agent manifest")
}

func TestHandlerMarkIndex_ForceOverridesManifest(t *testing.T) {
	sc := &stubClient{
		fetchFn: func(_, path string) (fetch.Result, error) {
			if path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": "sha256-aaaa"},
				Body:     "content",
			}}, nil
		},
		listFn: func(_, path string) (fetch.Result, error) {
			if path == "/" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "- [a.md](a.md)\n"}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   "created",
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}

	h := &handler{client: sc, token: "test-token"}
	result, err := h.markIndex(context.Background(), newCallToolRequest(map[string]any{
		"source":           "mark://source.com",
		"target":           "mark://hub.com/indexes/source.md",
		"expected_version": float64(0),
		"force":            true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "warning") {
		t.Error("should include warning about missing manifest")
	}
}

func TestHandlerMarkIndex_SourceNoManifestWarns(t *testing.T) {
	hash := "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sc := &stubClient{
		fetchFn: func(host, path string) (fetch.Result, error) {
			if path == protocol.WellKnownManifestPath {
				if host == "source.com:6309" {
					return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
				}
				// Target has manifest.
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Hub"}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": hash},
				Body:     "content",
			}}, nil
		},
		listFn: func(_, path string) (fetch.Result, error) {
			if path == "/" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "- [a.md](a.md)\n"}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: "created", Metadata: map[string]string{"version": "1"}}}, nil
		},
	}

	h := &handler{client: sc, token: "test-token"}
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
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "warning: source") {
		t.Error("should warn about source having no manifest")
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

func TestIsValidHash(t *testing.T) {
	tests := []struct {
		hash string
		want bool
	}{
		{"sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true},
		{"sha256-0000000000000000000000000000000000000000000000000000000000000000", true},
		{"sha256-AAAA", false},   // uppercase
		{"sha256-a1b2c3", false}, // too short
		{"md5-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", false},      // wrong prefix
		{"sha256-g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", false},   // invalid hex char
		{"sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2aa", false}, // too long
		{"", false},
	}
	for _, tt := range tests {
		if got := isValidHash(tt.hash); got != tt.want {
			t.Errorf("isValidHash(%q) = %v, want %v", tt.hash, got, tt.want)
		}
	}
}

func TestMarkBacklinksTool_URLRequired(t *testing.T) {
	tool := markBacklinksTool("mark://localhost:6309")
	props := tool.InputSchema.Properties
	if _, ok := props["url"]; !ok {
		t.Fatal("expected url property")
	}
	if !slices.Contains(tool.InputSchema.Required, "url") {
		t.Error("url should be required")
	}
}

func TestHandlerMarkBacklinks_BarePathWithoutHost(t *testing.T) {
	h := &handler{}
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

	h := &handler{graphStore: gs}
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
	if !strings.Contains(text.Text, "mark://host:6309/a.md") {
		t.Errorf("expected backlink URL in output: %s", text.Text)
	}
}

func TestHandlerMarkBacklinks_NilStore(t *testing.T) {
	h := &handler{defaultHost: "mark://host:6309"}
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

	h := &handler{graphStore: gs}
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
	if !strings.Contains(text.Text, "mark://host:6309/a.md") {
		t.Error("expected node URL in output")
	}
	if !strings.Contains(text.Text, "## Edges") {
		t.Error("expected edges section in output")
	}
}

func TestHandlerMarkGraphExport_NilStore(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markGraphExport(ctx, newCallToolRequest(nil))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "graph store not available")
}

func TestToolDefinition_MarkGraphPublish(t *testing.T) {
	tool := markGraphPublishTool("")
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
		publishFn: func(_, _, body, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			publishedBody = body
			return fetch.Result{Response: protocol.Response{
				Status:   "created",
				Metadata: map[string]string{"version": "1", "modified": "2026-03-08T12:00:00Z"},
			}}, nil
		},
	}

	h := &handler{client: sc, graphStore: gs, token: "test-token"}
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
	if !strings.Contains(publishedBody, "mark://host:6309/a.md") {
		t.Error("published body should contain node URLs")
	}
}

func TestHandlerMarkGraphPublish_NilStore(t *testing.T) {
	h := &handler{}
	ctx := context.Background()

	result, err := h.markGraphPublish(ctx, newCallToolRequest(map[string]any{
		"url":              "mark://target.com/graph.md",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "graph store not available")
}

func TestHandlerMarkGraphPublish_NoToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	h := &handler{graphStore: gs}
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
