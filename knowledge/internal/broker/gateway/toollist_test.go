package gateway

import (
	"flag"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

var updateToolGoldens = flag.Bool("update", false, "rewrite the tools/list goldens under testdata")

// TestToolListGolden pins tools/list of both gateway profiles; the lean tool
// profile is a filter TestGatewayToolProfilesAndBudget checks.
func TestToolListGolden(t *testing.T) {
	for name, profile := range map[string]*Profile{"knowledge": KnowledgeProfile(), "memory": MemoryProfile()} {
		t.Run(name, func(t *testing.T) {
			mcpfmttest.ToolsGolden(t, "testdata/tools-"+name+".json", profile.Tools, *updateToolGoldens)
		})
	}
}
