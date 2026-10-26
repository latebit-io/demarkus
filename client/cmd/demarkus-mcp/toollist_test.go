package main

import (
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

// TestAgentSurfaceGolden pins tools/list and the server instructions with and
// without -host, and the lean list the plugin runs with (-host), which
// tools/evalmocks serves to the eval; TestToolProfiles checks lean is a filter.
func TestAgentSurfaceGolden(t *testing.T) {
	for name, host := range map[string]string{"host": exampleHost, "nohost": ""} {
		t.Run(name, func(t *testing.T) {
			mcpfmttest.ToolsGolden(t, "testdata/tools-"+name+".json", profileSchemas(host, mcpfmt.ProfileFull))
			mcpfmttest.Golden(t, "testdata/instructions-"+name+".txt", []byte(serverInstructions(host)+"\n"))
			mcpfmttest.ServedToolsOnly(t, serverInstructions(host), profileSchemas(host, mcpfmt.ProfileFull))
		})
	}
	mcpfmttest.ToolsGolden(t, "testdata/tools-host-lean.json", profileSchemas(exampleHost, mcpfmt.ProfileLean))
	mcpfmttest.ServedToolsOnly(t, serverInstructions(exampleHost), profileSchemas(exampleHost, mcpfmt.ProfileLean))
}
