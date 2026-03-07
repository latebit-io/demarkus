package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/latebit/demarkus/client/internal/fetch"
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
	versionsFn func(host, path string) (fetch.Result, error)
	appendFn   func(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
}

func (s *stubClient) Fetch(_, _ string) (fetch.Result, error) { return fetch.Result{}, nil }
func (s *stubClient) List(_, _ string) (fetch.Result, error)  { return fetch.Result{}, nil }
func (s *stubClient) Publish(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
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
