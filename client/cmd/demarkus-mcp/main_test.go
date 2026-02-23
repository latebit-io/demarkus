package main

import (
	"context"
	"slices"
	"strings"
	"testing"

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
			wantRequired: []string{"url", "body"},
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
