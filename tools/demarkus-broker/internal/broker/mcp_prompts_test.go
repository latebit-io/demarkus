package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func gatewayPromptText(t *testing.T, fn func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error), args map[string]string) string {
	t.Helper()
	res, err := fn(context.Background(), mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{Arguments: args},
	})
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

func TestGatewayPromptsList(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	resp := g.mcpServer.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`))
	result, ok := resp.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("prompts/list returned %T", resp)
	}
	list, ok := result.Result.(mcp.ListPromptsResult)
	if !ok {
		t.Fatalf("result is %T, want ListPromptsResult", result.Result)
	}
	got := make(map[string]bool, len(list.Prompts))
	for _, p := range list.Prompts {
		got[p.Name] = true
	}
	for _, want := range []string{"orient", "recall", "whats-new"} {
		if !got[want] {
			t.Errorf("prompts/list missing %q; got %v", want, got)
		}
	}
}

func TestGatewayOrientPrompt(t *testing.T) {
	text := gatewayPromptText(t, orientPrompt, map[string]string{"url": "mark://team-a/plans/foo.md"})
	for _, want := range []string{"mark_explore", `"mark://team-a/plans/foo.md"`, "#<anchor>", "mark_fetch"} {
		if !strings.Contains(text, want) {
			t.Errorf("orient prompt missing %q in:\n%s", want, text)
		}
	}
}

func TestGatewayRecallPromptLooksUpAllWorlds(t *testing.T) {
	text := gatewayPromptText(t, recallPrompt, map[string]string{"subject": "escrow rules"})
	for _, want := range []string{"mark_lookup_all", `"escrow rules"`, `mark://<world>/<path>`, "status is partial", "never invent organizational memory"} {
		if !strings.Contains(text, want) {
			t.Errorf("recall prompt missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "mark_worlds") {
		t.Error("recall prompt should use broker-side aggregation")
	}
	for _, want := range []string{"partial lookup with no matches is inconclusive", "successful non-partial empty broader retry"} {
		if !strings.Contains(text, want) {
			t.Errorf("recall prompt missing empty-result guard %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "If no catalog has anything on the subject, say so plainly") {
		t.Errorf("recall prompt contains unconditional empty-result conclusion:\n%s", text)
	}
}

func TestGatewayWhatsNewPrompt(t *testing.T) {
	t.Run("universe-wide default", func(t *testing.T) {
		text := gatewayPromptText(t, whatsNewPrompt, nil)
		for _, want := range []string{"7 days before today", "mark_lookup_all", `query "*"`, "modified-after=", "status is partial", "modified timestamp", "not exhaustive", "partial lookup with no matches is inconclusive"} {
			if !strings.Contains(text, want) {
				t.Errorf("whats-new prompt missing %q in:\n%s", want, text)
			}
		}
		exploreAt := strings.Index(text, "mark_explore")
		fetchAt := strings.Index(text, "mark_fetch")
		if exploreAt < 0 || fetchAt < 0 || exploreAt >= fetchAt {
			t.Errorf("whats-new must explore before targeted fetch; explore=%d fetch=%d in:\n%s", exploreAt, fetchAt, text)
		}
		if strings.Contains(text, "If nothing changed, say so") {
			t.Errorf("whats-new contains unconditional empty-result conclusion:\n%s", text)
		}
	})
	t.Run("world-scoped with since", func(t *testing.T) {
		text := gatewayPromptText(t, whatsNewPrompt, map[string]string{"since": "2026-07-01", "world": "servicing"})
		for _, want := range []string{`"2026-07-01"`, `mark://servicing/`} {
			if !strings.Contains(text, want) {
				t.Errorf("scoped whats-new missing %q in:\n%s", want, text)
			}
		}
		if strings.Contains(text, "mark_lookup_all") {
			t.Error("world-scoped whats-new should use targeted lookup")
		}
		if !strings.Contains(text, "successful empty targeted lookup") {
			t.Errorf("world-scoped whats-new missing empty-result guard in:\n%s", text)
		}
	})
}

func TestGatewayPromptsRequireArguments(t *testing.T) {
	tests := []struct {
		name string
		fn   func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error)
		arg  string
	}{
		{"orient", orientPrompt, "url"},
		{"recall", recallPrompt, "subject"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.fn(context.Background(), mcp.GetPromptRequest{
				Params: mcp.GetPromptParams{Arguments: map[string]string{tt.arg: "  "}},
			})
			if err == nil || !strings.Contains(err.Error(), tt.arg) {
				t.Errorf("expected error naming %q, got %v", tt.arg, err)
			}
		})
	}
}
