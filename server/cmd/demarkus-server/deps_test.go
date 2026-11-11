package main

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/depstest"
)

// TestLocalMainCarriesNoCloud pins ADR 0037 decision 6: the soul's binary is
// the local profile of the shared runtime and never links a cloud client,
// gRPC, telemetry or OAuth, whatever the knowledge server grows.
func TestLocalMainCarriesNoCloud(t *testing.T) {
	depstest.Forbid(t, ".")
}
