package main

import (
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/mark3labs/mcp-go/mcp"
)

// Wire bytes of tools/list per profile with five percent headroom; o200k is
// about 4.1 bytes per token (2026-09-17). Raise only with a measured offset.
const (
	fullSchemaBudgetBytes = 10700
	leanSchemaBudgetBytes = 7400
)

func profileSchemas(profile string) []mcp.Tool {
	h := &handler{client: &stubClient{}}
	entries := h.profileTools("mark://example.com:6309", profile)
	tools := make([]mcp.Tool, 0, len(entries))
	for i := range entries {
		tools = append(tools, entries[i].Tool)
	}
	return tools
}

func TestToolProfiles(t *testing.T) {
	full := profileSchemas(mcpfmt.ProfileFull)
	lean := profileSchemas(mcpfmt.ProfileLean)
	if len(full) != 15 {
		t.Fatalf("full profile = %d tools, want 15", len(full))
	}
	if len(lean) != len(full)-len(mcpfmt.AdvancedTools) {
		t.Fatalf("lean profile = %d tools, want %d", len(lean), len(full)-len(mcpfmt.AdvancedTools))
	}
	for _, tool := range lean {
		if mcpfmt.AdvancedTools[tool.Name] {
			t.Errorf("lean profile exposes advanced tool %s", tool.Name)
		}
	}
}

func TestToolSchemaBudget(t *testing.T) {
	for profile, budget := range map[string]int{mcpfmt.ProfileFull: fullSchemaBudgetBytes, mcpfmt.ProfileLean: leanSchemaBudgetBytes} {
		if err := mcpfmt.CheckSchemaBudget(profileSchemas(profile), budget); err != nil {
			t.Errorf("%s: %v", profile, err)
		}
	}
}

func TestProfileDefaultFromEnvironment(t *testing.T) {
	t.Setenv(profileEnv, "")
	if got := profileDefault(); got != mcpfmt.ProfileFull {
		t.Fatalf("default = %q, want full", got)
	}
	t.Setenv(profileEnv, mcpfmt.ProfileLean)
	if got := profileDefault(); got != mcpfmt.ProfileLean {
		t.Fatalf("default = %q, want lean from %s", got, profileEnv)
	}
}
