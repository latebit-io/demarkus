package broker

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

// gatewayResourceURIs drives a real resources/list request through the
// gateway's MCP server and returns the URIs as a set.
func gatewayResourceURIs(t *testing.T, s *mcpserver.MCPServer) map[string]bool {
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

func TestGatewayResources_PerWorldHubsAndTemplate(t *testing.T) {
	g := newGatewayWithDispatcher(t, worldsTestConfig(), &fakeDispatcher{})

	got := gatewayResourceURIs(t, g.mcpServer)
	for _, want := range []string{"mark://team-a/index.md", "mark://secret-b/index.md"} {
		if !got[want] {
			t.Errorf("resources/list missing world hub %s; got %v", want, got)
		}
	}

	resp := g.mcpServer.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"resources/templates/list"}`))
	result, ok := resp.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("templates/list returned %T", resp)
	}
	list, ok := result.Result.(mcp.ListResourceTemplatesResult)
	if !ok {
		t.Fatalf("result is %T, want ListResourceTemplatesResult", result.Result)
	}
	if len(list.ResourceTemplates) != 1 || list.ResourceTemplates[0].URITemplate.Raw() != "mark://{world}/{+path}" {
		t.Errorf("expected the mark://{world}/{+path} template, got %+v", list.ResourceTemplates)
	}
}

func gatewayReadResourceText(t *testing.T, g *mcpGateway, uri string) string {
	t.Helper()
	contents, err := g.readResource(context.Background(), mcp.ReadResourceRequest{
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

func TestGatewayReadResource_WholeAndSection(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "3", "abc"))

	whole := gatewayReadResourceText(t, g, "mark://team-a/doc.md")
	if whole != fetchModeDoc {
		t.Errorf("whole read should return the exact body, got:\n%s", whole)
	}

	section := gatewayReadResourceText(t, g, "mark://team-a/doc.md#setup")
	if !strings.HasPrefix(section, "## Setup") || !strings.Contains(section, "Setup body.") {
		t.Errorf("section read should return the section, got:\n%s", section)
	}
	if strings.Contains(section, "Usage body.") {
		t.Error("section read should not include other sections")
	}
}

func TestGatewayReadResource_LargeDocIsNotOutlined(t *testing.T) {
	// Attaching is deliberate: resource reads never outline-gate and never
	// hit the session dedup, regardless of size or fetch history.
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(bigFetchModeDoc(), "3", "abc"))
	first := gatewayReadResourceText(t, g, "mark://team-a/big.md")
	if !strings.Contains(first, "filler line") {
		t.Fatal("resource read of a large doc must return the full body")
	}
	if second := gatewayReadResourceText(t, g, "mark://team-a/big.md"); second != first {
		t.Error("repeat resource read must return the same full body (no dedup)")
	}
}

func TestGatewayReadResource_Errors(t *testing.T) {
	tests := []struct {
		name       string
		dispatcher worldDispatcher
		uri        string
		wantErr    string
	}{
		{
			"section not found lists anchors",
			fetchModeDispatcher(fetchModeDoc, "3", "abc"),
			"mark://team-a/doc.md#nope",
			"available anchors",
		},
		{
			"non-ok status surfaces",
			&fakeDispatcher{fetchFn: func(_, _, _ string) (fetch.Result, error) {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}},
			"mark://team-a/missing.md",
			"not-found",
		},
		{
			"transport error surfaces",
			&fakeDispatcher{fetchFn: func(_, _, _ string) (fetch.Result, error) {
				return fetch.Result{}, fmt.Errorf("boom")
			}},
			"mark://team-a/doc.md",
			"boom",
		},
		{
			"missing world name",
			&fakeDispatcher{},
			"mark:///no-world.md",
			"missing world name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGatewayWithDispatcher(t, mcpTestConfig(), tt.dispatcher)
			_, err := g.readResource(context.Background(), mcp.ReadResourceRequest{
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

func TestGatewayReadResource_BinaryNotice(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(binaryFetchModeDoc, "1", "abc"))
	contents, err := g.readResource(context.Background(), mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "mark://team-a/img.png"},
	})
	if err != nil {
		t.Fatalf("readResource: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(contents))
	}
	text, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("contents[0] is %T, want TextResourceContents", contents[0])
	}
	if text.MIMEType != "text/plain" {
		t.Errorf("MIMEType = %q, want text/plain", text.MIMEType)
	}
	if !strings.Contains(text.Text, "non-markdown or binary document") {
		t.Errorf("binary body should return the notice, got:\n%s", text.Text)
	}
	if strings.Contains(text.Text, "\x89PNG") {
		t.Error("binary bytes must not be rendered into the resource")
	}
}
