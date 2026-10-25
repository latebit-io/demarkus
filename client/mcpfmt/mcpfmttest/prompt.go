package mcpfmttest

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// PromptText calls a prompt handler with args and returns the one user text
// message every MCP surface's prompts answer with.
func PromptText(t testing.TB, handler func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error), args map[string]string) string {
	t.Helper()
	res, err := handler(context.Background(), mcp.GetPromptRequest{Params: mcp.GetPromptParams{Arguments: args}})
	if err != nil {
		t.Fatalf("prompt handler: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(res.Messages))
	}
	content, ok := res.Messages[0].Content.(mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want TextContent", res.Messages[0].Content)
	}
	if res.Messages[0].Role != mcp.RoleUser {
		t.Errorf("role = %q, want user", res.Messages[0].Role)
	}
	return content.Text
}
