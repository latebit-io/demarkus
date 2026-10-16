package main

import (
	"flag"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

var updateToolGoldens = flag.Bool("update", false, "rewrite the tools/list goldens under testdata")

// TestToolListGolden pins tools/list with and without -host; the plugin runs
// with -host and the lean profile, which TestToolProfiles checks is a filter.
func TestToolListGolden(t *testing.T) {
	for name, host := range map[string]string{"host": "mark://localhost:6309", "nohost": ""} {
		t.Run(name, func(t *testing.T) {
			tools := profileSchemas(host, mcpfmt.ProfileFull)
			mcpfmttest.ToolsGolden(t, "testdata/tools-"+name+".json", tools, *updateToolGoldens)
		})
	}
}
