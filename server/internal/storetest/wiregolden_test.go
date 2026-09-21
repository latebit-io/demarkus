package storetest

import (
	"flag"
	"testing"
)

var updateWireGoldens = flag.Bool("update", false, "rewrite the wire goldens under protocol/wiretest/testdata")

// TestFileStoreWireGoldens pins the per verb wire fixtures that client and
// broker tests consume through protocol/wiretest.
func TestFileStoreWireGoldens(t *testing.T) {
	RunWireGoldens(t, FileBackend(t), "../../../protocol/wiretest/testdata", *updateWireGoldens)
}
