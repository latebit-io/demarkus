package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func readResourceText(t *testing.T, h *handler, uri string) string {
	t.Helper()
	contents, err := h.readResource(context.Background(), mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: uri},
	})
	if err != nil {
		t.Fatalf("readResource(%s): %v", uri, err)
	}
	if len(contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(contents))
	}
	text, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("contents[0] is %T, want TextResourceContents", contents[0])
	}
	if text.MIMEType != "text/markdown" {
		t.Errorf("MIMEType = %q, want text/markdown", text.MIMEType)
	}
	if text.URI != uri {
		t.Errorf("URI = %q, want %q (echoed verbatim)", text.URI, uri)
	}
	return text.Text
}

func TestReadResource_WholeDocument(t *testing.T) {
	h := &handler{client: fetchStub(smallDoc, "3", "abc", nil)}
	text := readResourceText(t, h, "mark://example.com/doc.md")
	if text != smallDoc {
		t.Errorf("resource read should return the exact body, got:\n%s", text)
	}
}

func TestReadResource_Section(t *testing.T) {
	h := &handler{client: fetchStub(smallDoc, "3", "abc", nil)}
	text := readResourceText(t, h, "mark://example.com/doc.md#setup")
	if !strings.HasPrefix(text, "## Setup") || !strings.Contains(text, "Setup body.") {
		t.Errorf("section resource should return the section, got:\n%s", text)
	}
	if strings.Contains(text, "Usage body.") {
		t.Error("section resource should not include other sections")
	}
}

func TestReadResource_LargeDocIsNotOutlined(t *testing.T) {
	// Attaching is deliberate: resource reads never outline-gate and never
	// hit the unchanged dedup, regardless of size or fetch history.
	h := &handler{client: fetchStub(bigDoc(), "3", "abc", nil)}
	first := readResourceText(t, h, "mark://example.com/big.md")
	if !strings.Contains(first, "filler line") {
		t.Fatal("resource read of a large doc must return the full body")
	}
	second := readResourceText(t, h, "mark://example.com/big.md")
	if second != first {
		t.Error("repeat resource read must return the same full body (no dedup)")
	}
}

func TestReadResource_Errors(t *testing.T) {
	tests := []struct {
		name    string
		client  markClient
		uri     string
		wantErr string
	}{
		{
			"section not found lists anchors",
			fetchStub(smallDoc, "3", "abc", nil),
			"mark://example.com/doc.md#nope",
			"available anchors",
		},
		{
			"non-ok status surfaces",
			&stubClient{fetchFn: func(_, _, _ string) (fetch.Result, error) {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}},
			"mark://example.com/missing.md",
			"not-found",
		},
		{
			"transport error surfaces",
			&stubClient{fetchFn: func(_, _, _ string) (fetch.Result, error) {
				return fetch.Result{}, fmt.Errorf("boom")
			}},
			"mark://example.com/doc.md",
			"boom",
		},
		{
			"bare path without host",
			&stubClient{},
			"/doc.md",
			"requires -host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &handler{client: tt.client}
			_, err := h.readResource(context.Background(), mcp.ReadResourceRequest{
				Params: mcp.ReadResourceParams{URI: tt.uri},
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestRegisterListedResources(t *testing.T) {
	listing := "- [index.md](index.md)\n- [patterns.md](patterns.md)\n- [journal/](journal/)\n- [notes.md](notes.md)\n- [image.png](image.png)\n"
	sc := &stubClient{
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: listing}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://example.com"}
	s := mcpserver.NewMCPServer("test", "0", mcpserver.WithResourceCapabilities(false, true))

	registerListedResources(s, h, "mark://example.com")

	// The registered resources are observable through a resources/list
	// round-trip on the server.
	got := listResourceURIs(t, s)
	for _, want := range []string{"mark://example.com/patterns.md", "mark://example.com/notes.md"} {
		if !got[want] {
			t.Errorf("listing should register %s; got %v", want, got)
		}
	}
	for _, not := range []string{"mark://example.com/index.md", "mark://example.com/journal/", "mark://example.com/image.png"} {
		if got[not] {
			t.Errorf("listing must not register %s (index dedup / dirs / non-markdown)", not)
		}
	}
}

func TestRegisterListedResources_HostDownIsQuiet(t *testing.T) {
	sc := &stubClient{
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{}, fmt.Errorf("dial: connection refused")
		},
	}
	h := &handler{client: sc, defaultHost: "mark://example.com"}
	s := mcpserver.NewMCPServer("test", "0", mcpserver.WithResourceCapabilities(false, true))

	registerListedResources(s, h, "mark://example.com") // must not panic or register anything
	if got := listResourceURIs(t, s); len(got) != 0 {
		t.Errorf("down host should register nothing, got %v", got)
	}
}

// listResourceURIs drives a real resources/list request through the
// server and returns the URIs as a set.
func listResourceURIs(t *testing.T, s *mcpserver.MCPServer) map[string]bool {
	t.Helper()
	resp := s.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`))
	result, ok := resp.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("resources/list returned %T", resp)
	}
	list, ok := result.Result.(mcp.ListResourcesResult)
	if !ok {
		t.Fatalf("result is %T, want ListResourcesResult", result.Result)
	}
	got := make(map[string]bool, len(list.Resources))
	for _, r := range list.Resources {
		got[r.URI] = true
	}
	return got
}

func TestRegisterResources_WellKnownAndTemplate(t *testing.T) {
	h := &handler{client: &stubClient{}, defaultHost: "mark://example.com"}
	s := mcpserver.NewMCPServer("test", "0")
	registerResources(s, h, "mark://example.com")

	got := listResourceURIs(t, s)
	for _, want := range []string{"mark://example.com/index.md", "mark://example.com" + protocol.WellKnownManifestPath} {
		if !got[want] {
			t.Errorf("well-known resource %s missing; got %v", want, got)
		}
	}
}
