package gateway

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
)

func TestMCPToolsExactly17(t *testing.T) {
	tools := mcpTools()
	if len(tools) != len(mcpToolNames) {
		t.Fatalf("mcpTools() returned %d tools, mcpToolNames lists %d — names + builders out of sync", len(tools), len(mcpToolNames))
	}
	// 15 tools mirror client/cmd/demarkus-mcp for cross-transport parity;
	// mark_worlds and mark_lookup_all are deliberate broker-only additions:
	// both operate on the knowledge system rather than one world.
	if len(tools) != 17 {
		t.Fatalf("expected 17 tools (demarkus-mcp parity surface + 2 broker tools), got %d", len(tools))
	}
	for i, tool := range tools {
		if tool.Name != mcpToolNames[i] {
			t.Errorf("tools[%d].Name = %q, mcpToolNames[%d] = %q", i, tool.Name, i, mcpToolNames[i])
		}
	}
}

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
// about 4.1 bytes per token (2026-09-17). Raise only with a measured offset.
const (
	knowledgeFullSchemaBudgetBytes = 12550
	knowledgeLeanSchemaBudgetBytes = 9400
	memoryFullSchemaBudgetBytes    = 8900
	memoryLeanSchemaBudgetBytes    = 8550
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

func TestMCPToolsExposeRequiredArguments(t *testing.T) {
	// Pin the contract the plan spells out: mark_publish requires
	// expected_version; mark_append leaves it optional (auto-resolved
	// via VERSIONS by the broker handler in Slice 3); mark_resolve
	// needs hash + index; mark_index needs source + target.
	tests := []struct {
		tool         string
		wantRequired []string
		wantOptional []string
	}{
		{
			tool:         "mark_fetch",
			wantRequired: []string{"url"},
			wantOptional: []string{"force", "verbose"},
		},
		{
			tool:         "mark_lookup",
			wantRequired: []string{"url", "query"},
			wantOptional: []string{"filter", "limit", "match", "verbose"},
		},
		{
			tool:         "mark_lookup_all",
			wantRequired: []string{"query"},
			wantOptional: []string{"scope", "filter", "limit", "match", "verbose"},
		},
		{
			tool:         "mark_explore",
			wantRequired: []string{"url"},
			wantOptional: []string{"verbose"},
		},
		{
			tool:         "mark_publish",
			wantRequired: []string{"url", "body", "expected_version"},
			wantOptional: []string{"on_conflict"},
		},
		{
			tool:         "mark_append",
			wantRequired: []string{"url", "body"},
			wantOptional: []string{"expected_version"},
		},
		{
			tool:         "mark_resolve",
			wantRequired: []string{"hash", "index"},
		},
		{
			tool:         "mark_index",
			wantRequired: []string{"source", "target"},
			wantOptional: []string{"expected_version", "dry_run", "force"},
		},
	}

	byName := map[string]map[string]bool{}
	requiredByName := map[string]map[string]bool{}
	for _, tool := range mcpTools() {
		props := schemaProperties(tool.InputSchema.Properties)
		byName[tool.Name] = props
		req := map[string]bool{}
		for _, r := range tool.InputSchema.Required {
			req[r] = true
		}
		requiredByName[tool.Name] = req
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			props := byName[tt.tool]
			req := requiredByName[tt.tool]
			for _, name := range tt.wantRequired {
				if !props[name] {
					t.Errorf("tool %q missing property %q", tt.tool, name)
				}
				if !req[name] {
					t.Errorf("tool %q: %q should be required", tt.tool, name)
				}
			}
			for _, name := range tt.wantOptional {
				if !props[name] {
					t.Errorf("tool %q missing optional property %q", tt.tool, name)
				}
				if req[name] {
					t.Errorf("tool %q: %q should be optional, not required", tt.tool, name)
				}
			}
		})
	}
}

// schemaProperties extracts the property names from a tool's input
// schema, regardless of whether the underlying mcp.ToolInputSchema
// stores them as map[string]any or another internal shape. Returns a
// set of property names for membership checks.
func schemaProperties(props map[string]any) map[string]bool {
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}
