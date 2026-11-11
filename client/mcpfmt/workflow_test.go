package mcpfmt

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func promptText(t *testing.T, h func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error), args map[string]string) (title, text string) {
	t.Helper()
	res, err := h(context.Background(), mcp.GetPromptRequest{Params: mcp.GetPromptParams{Arguments: args}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Description, res.Messages[0].Content.(mcp.TextContent).Text
}

// One procedure, two wordings: what a surface supplies shows up in its
// place and nowhere else, and a surface without worlds has no world argument.
func TestWorkflowPrompts(t *testing.T) {
	single := Workflow{Document: "DOC", Store: "STORE", Lookup: "LOOKUP %q.", Fetch: "FETCH.", Report: "REPORT.", Where: "WHERE", Changed: "CHANGED.", Read: "READ.", Digest: "DIGEST.", Empty: "EMPTY."}
	worlds := single
	worlds.ChangedIn, worlds.EmptyIn = "CHANGED IN %s.", "EMPTY IN."

	prompts := single.Prompts()
	if len(prompts) != 3 || prompts[0].Prompt.Name != "orient" || prompts[1].Prompt.Name != "recall" || prompts[2].Prompt.Name != "whats-new" {
		t.Fatalf("prompts = %+v", prompts)
	}
	if n := len(prompts[2].Prompt.Arguments); n != 1 {
		t.Errorf("whats-new without worlds takes %d arguments, want since alone", n)
	}
	if n := len(worlds.Prompts()[2].Prompt.Arguments); n != 2 {
		t.Errorf("whats-new with worlds takes %d arguments, want since and world", n)
	}

	if _, text := promptText(t, single.Orient, map[string]string{"url": "/a.md"}); !strings.HasPrefix(text, "Orient around the DOC /a.md.\n\n1. Call mark_explore with url \"/a.md\";") {
		t.Errorf("orient = %s", text)
	}
	title, text := promptText(t, single.Recall, map[string]string{"subject": "auth"})
	if title != "Recall: auth" || !strings.HasPrefix(text, "Recall what this STORE knows about: auth.\n\n1. LOOKUP \"auth\".\n2. Call mark_explore") || !strings.HasSuffix(text, "\n3. FETCH.\n4. REPORT.") {
		t.Errorf("recall = %q %s", title, text)
	}
	// A surface without worlds ignores a world argument a stale client sends.
	title, text = promptText(t, single.WhatsNew, map[string]string{"world": "team"})
	if title != "What's new" || !strings.HasSuffix(text, "1. Use the date 7 days before today, formatted YYYY-MM-DD.\n2. CHANGED.\n3. READ.\n4. DIGEST. EMPTY.") {
		t.Errorf("whats-new = %q %s", title, text)
	}
	title, text = promptText(t, worlds.WhatsNew, map[string]string{"world": "team", "since": "2026-01-01"})
	if title != "What's new since 2026-01-01 in team" || !strings.HasSuffix(text, "1. Use the date \"2026-01-01\".\n2. CHANGED IN team.\n3. READ.\n4. DIGEST. EMPTY IN.") {
		t.Errorf("scoped whats-new = %q %s", title, text)
	}
	for name, h := range map[string]func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error){"url": single.Orient, "subject": single.Recall} {
		if _, err := h(context.Background(), mcp.GetPromptRequest{Params: mcp.GetPromptParams{Arguments: map[string]string{name: "  "}}}); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("blank %s: err = %v, want it named", name, err)
		}
	}
}
