package broker

import (
	"strings"
	"testing"
)

func TestMCPToolsExactly15(t *testing.T) {
	tools := mcpTools()
	if len(tools) != len(mcpToolNames) {
		t.Fatalf("mcpTools() returned %d tools, mcpToolNames lists %d — names + builders out of sync", len(tools), len(mcpToolNames))
	}
	// 14 tools mirror client/cmd/demarkus-mcp for cross-transport parity;
	// mark_worlds is the one deliberate broker-only addition (enumerating
	// worlds is a knowledge-system concept with no single-world analogue).
	if len(tools) != 15 {
		t.Fatalf("expected 15 tools (demarkus-mcp parity surface + mark_worlds), got %d", len(tools))
	}
	for i, tool := range tools {
		if tool.Name != mcpToolNames[i] {
			t.Errorf("tools[%d].Name = %q, mcpToolNames[%d] = %q", i, tool.Name, i, mcpToolNames[i])
		}
	}
}

func TestMCPToolDescriptionsReferenceBrokerURLForm(t *testing.T) {
	// The broker addresses worlds by name, not host:port. Every tool
	// that takes a URL argument must spell out the mark://{worldName}/
	// form in its description so the LLM picks the right shape.
	// mark_graph_export is the lone exception (no URL argument).
	for _, tool := range mcpTools() {
		if tool.Name == "mark_graph_export" {
			continue
		}
		if !strings.Contains(tool.Description, "mark://") {
			t.Errorf("tool %q description missing mark:// URL hint: %q", tool.Name, tool.Description)
		}
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
