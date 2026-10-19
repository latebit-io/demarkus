package procscan

import (
	"os"
	"strconv"
	"testing"
)

// This test binary carries no -port flag, so it reads as the protocol default;
// any other port is a mismatch, and a process that is gone never is.
func TestListensOnComparesTheProbedPort(t *testing.T) {
	t.Setenv("DEMARKUS_PORT", "")
	pid := os.Getpid()
	if !ListensOn(pid, strconv.Itoa(demarkusProtocolPort)) {
		t.Error("own process on the default port read as a mismatch")
	}
	if ListensOn(pid, "1") {
		t.Error("port 1 read as a match")
	}
	if !ListensOn(1<<30, "1") {
		t.Error("a process that is gone must not read as a mismatch")
	}
}
