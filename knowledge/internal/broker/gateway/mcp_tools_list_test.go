package gateway

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
)

func TestMCPURLFormStatedOnceInInstructions(t *testing.T) {
	// The broker addresses worlds by name. The URL form rides the server
	// instructions once; every URL argument still shows a mark:// example so
	// the model picks the right shape without a per-tool description suffix.
	for _, instructions := range []string{knowledgeInstructions, memoryInstructions} {
		if !strings.Contains(instructions, "mark://{worldName}/{path}") {
			t.Fatalf("instructions must state the URL form: %q", instructions[:80])
		}
	}
	for _, tool := range mcpTools() {
		if strings.Contains(tool.Description, "URLs: mark://") {
			t.Errorf("tool %q repeats the URL hint in its description", tool.Name)
		}
		for name, prop := range tool.InputSchema.Properties {
			desc, _ := prop.(map[string]any)["description"].(string)
			if strings.Contains(name, "url") && !strings.Contains(desc, "mark://") {
				t.Errorf("tool %q argument %q lacks a mark:// example: %q", tool.Name, name, desc)
			}
		}
	}
}

// Wire bytes of tools/list per profile with five percent headroom; o200k is
// about 4.1 bytes per token (2026-10-12). Raise only with a measured offset.
const (
	knowledgeFullSchemaBudgetBytes = 14630
	knowledgeLeanSchemaBudgetBytes = 13200
	memoryFullSchemaBudgetBytes    = 12410
	memoryLeanSchemaBudgetBytes    = 11980
)

func TestGatewayToolProfilesAndBudget(t *testing.T) {
	cases := []struct {
		name                   string
		profile                *Profile
		fullBudget, leanBudget int
	}{
		{"knowledge", KnowledgeProfile(), knowledgeFullSchemaBudgetBytes, knowledgeLeanSchemaBudgetBytes},
		{"memory", MemoryProfile(), memoryFullSchemaBudgetBytes, memoryLeanSchemaBudgetBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			advanced := 0
			for _, tool := range tc.profile.Tools {
				if mcpfmt.AdvancedTools[tool.Name] {
					advanced++
				}
			}
			full := mcpfmt.ProfileTools(mcpfmt.ProfileFull, tc.profile.Tools)
			lean := mcpfmt.ProfileTools(mcpfmt.ProfileLean, tc.profile.Tools)
			if len(full) != len(tc.profile.Tools) || len(lean) != len(tc.profile.Tools)-advanced {
				t.Fatalf("full = %d, lean = %d, want %d and %d", len(full), len(lean), len(tc.profile.Tools), len(tc.profile.Tools)-advanced)
			}
			for _, tool := range lean {
				if mcpfmt.AdvancedTools[tool.Name] {
					t.Errorf("lean profile exposes %s", tool.Name)
				}
			}
			if err := mcpfmt.CheckSchemaBudget(full, tc.fullBudget); err != nil {
				t.Error(err)
			}
			if err := mcpfmt.CheckSchemaBudget(lean, tc.leanBudget); err != nil {
				t.Error(err)
			}
		})
	}
}
