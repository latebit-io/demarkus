package main

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/depstest"
)

// TestClientBinariesCarryNoServer pins the client binaries (CLI, MCP, TUI) to
// the store client: no server module, cloud client, gRPC, telemetry or SDK.
func TestClientBinariesCarryNoServer(t *testing.T) {
	depstest.Forbid(t, "../...", "github.com/latebit-io/demarkus/server")
}
