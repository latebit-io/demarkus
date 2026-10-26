package main

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestOrientPrompt(t *testing.T) {
	text := mcpfmttest.PromptText(t, orientPrompt, map[string]string{"url": "/plans/foo.md"})
	for _, want := range []string{"mark_explore", `"/plans/foo.md"`, "#<anchor>", "mark_fetch"} {
		if !strings.Contains(text, want) {
			t.Errorf("orient prompt missing %q in:\n%s", want, text)
		}
	}
}

func TestRecallPrompt(t *testing.T) {
	text := mcpfmttest.PromptText(t, recallPrompt, map[string]string{"subject": "broker auth"})
	for _, want := range []string{"mark_lookup", `"broker auth"`, "mark_explore", "never invent memory"} {
		if !strings.Contains(text, want) {
			t.Errorf("recall prompt missing %q in:\n%s", want, text)
		}
	}
}

func TestWhatsNewPrompt(t *testing.T) {
	t.Run("default window", func(t *testing.T) {
		text := mcpfmttest.PromptText(t, whatsNewPrompt, nil)
		for _, want := range []string{"7 days before today", `query "*"`, "modified-after="} {
			if !strings.Contains(text, want) {
				t.Errorf("whats-new prompt missing %q in:\n%s", want, text)
			}
		}
	})
	t.Run("explicit since", func(t *testing.T) {
		text := mcpfmttest.PromptText(t, whatsNewPrompt, map[string]string{"since": "2026-07-01"})
		if !strings.Contains(text, `"2026-07-01"`) {
			t.Errorf("whats-new prompt should carry the given date:\n%s", text)
		}
		if strings.Contains(text, "7 days before today") {
			t.Error("explicit since should replace the default window")
		}
	})
}

func TestPromptsRequireArguments(t *testing.T) {
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

func TestRegisterPromptsListsAll(t *testing.T) {
	got := mcpserverForPrompts(t).ListPrompts()
	for _, want := range []string{"orient", "recall", "whats-new"} {
		if got[want] == nil {
			t.Errorf("prompts/list missing %q", want)
		}
	}
}

// Every embedded command is listed; LoadPrompts fails on a file it skips.
func TestSoulPromptsNeedHostAndFullProfile(t *testing.T) {
	prompts, err := mcpfmt.LoadPrompts(soulPromptFS, "prompts/memory/commands")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		host, profile string
		want          bool
	}{
		{"mark://example.com", mcpfmt.ProfileFull, true},
		{"mark://example.com", mcpfmt.ProfileLean, false},
		{"", mcpfmt.ProfileFull, false},
	} {
		s := mcpserver.NewMCPServer("test", "0")
		registerPrompts(s, tt.host, tt.profile)
		got := s.ListPrompts()
		for _, p := range prompts {
			name := p.Prompt.Name
			if (got[name] != nil) != tt.want {
				t.Errorf("host %q profile %s: %s listed = %v, want %v", tt.host, tt.profile, name, got[name] != nil, tt.want)
			}
		}
	}
}

func TestSoulJournalAddressesBarePaths(t *testing.T) {
	journal := mcpserverForPrompts(t).ListPrompts()["soul-journal"]
	text := mcpfmttest.PromptText(t, journal.Handler, map[string]string{mcpfmt.PromptArgument: "shipped"})
	if strings.Contains(text, "mark_worlds") || !strings.Contains(text, "`/` for the root project") || !strings.HasSuffix(text, "Entry: shipped\n") {
		t.Errorf("soul-journal must address bare paths and fill its entry:\n%s", text)
	}
}

// mcpserverForPrompts builds a server with the prompts registered.
func mcpserverForPrompts(t *testing.T) *mcpserver.MCPServer {
	t.Helper()
	s := mcpserver.NewMCPServer("test", "0")
	registerPrompts(s, "mark://example.com", mcpfmt.ProfileFull)
	return s
}
