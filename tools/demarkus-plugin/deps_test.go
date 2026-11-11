package main

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/depstest"
)

// TestPluginCarriesNoCloud pins the plugin binary, which every session runs,
// to no cloud client, gRPC, telemetry or SDK, whatever tools/go.mod requires.
func TestPluginCarriesNoCloud(t *testing.T) {
	depstest.Forbid(t, ".")
}
