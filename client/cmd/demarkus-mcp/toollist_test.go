package main

import (
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

// TestToolListGolden pins tools/list with and without -host; the plugin runs
// with -host and the lean profile, which TestToolProfiles checks is a filter.
func TestToolListGolden(t *testing.T) {
	for name, host := range map[string]string{"host": exampleHost, "nohost": ""} {
		t.Run(name, func(t *testing.T) {
			tools := profileSchemas(host, mcpfmt.ProfileFull)
			mcpfmttest.ToolsGolden(t, "testdata/tools-"+name+".json", tools)
		})
	}
}
