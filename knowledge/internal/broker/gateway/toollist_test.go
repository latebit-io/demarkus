package gateway

import (
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

// TestToolListGolden pins tools/list of both gateway profiles; the lean tool
// profile is a filter TestGatewayToolProfilesAndBudget checks.
func TestToolListGolden(t *testing.T) {
	for name, profile := range map[string]*Profile{"knowledge": KnowledgeProfile(), "memory": MemoryProfile()} {
		t.Run(name, func(t *testing.T) {
			mcpfmttest.ToolsGolden(t, "testdata/tools-"+name+".json", profile.Tools)
		})
	}
}
