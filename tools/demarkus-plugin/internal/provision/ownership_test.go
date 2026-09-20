package provision

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// With ps unavailable the probe cannot answer; that must read as unknown, never
// as "not ours", or a live managed server loses its pid file and gets a twin.
func TestProbeServerAtRootUnknownWhenProbeCannotRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := probeServerAtRoot(os.Getpid(), "/some/root"); got != ownershipUnknown {
		t.Fatalf("probeServerAtRoot = %v, want ownershipUnknown", got)
	}
	if got := probeServerAtRoot(0, "/some/root"); got != ownershipNo {
		t.Fatalf("pid 0 = %v, want ownershipNo", got)
	}
}

func TestEnsureManagedServerAbortsOnUnknownOwnership(t *testing.T) {
	memoryDir := t.TempDir()
	pidFile := filepath.Join(memoryDir, ".pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	t.Setenv("PATH", t.TempDir())

	if err := ensureManagedServer(memoryDir, 6309); err == nil {
		t.Fatal("ensureManagedServer proceeded without knowing who owns the recorded pid")
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("pid file was cleared on an unknown probe: %v", err)
	}
}
