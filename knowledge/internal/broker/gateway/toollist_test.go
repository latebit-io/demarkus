package gateway

import (
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

// TestAgentSurfaceGolden pins tools/list and the server instructions of both
// gateway profiles; the lean tool profile is a filter
// TestGatewayToolProfilesAndBudget checks.
func TestAgentSurfaceGolden(t *testing.T) {
	for name, profile := range map[string]*Profile{"knowledge": KnowledgeProfile(), "memory": MemoryProfile()} {
		t.Run(name, func(t *testing.T) {
			mcpfmttest.ToolsGolden(t, "testdata/tools-"+name+".json", profile.Tools)
			mcpfmttest.Golden(t, "testdata/instructions-"+name+".txt", []byte(profile.Instructions+"\n"))
			mcpfmttest.ServedToolsOnly(t, profile.Instructions, profile.Tools)
		})
	}
}

// knowledgeToolNames lists the tools the knowledge profile serves.
func knowledgeToolNames() []string {
	tools := KnowledgeProfile().Tools
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].Name
	}
	return names
}
