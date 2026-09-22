package server

import (
	"strings"
	"syscall"

	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/provisiontest"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/release"
)

func TestEnsureAbortsOnUnknownOwnership(t *testing.T) {
	memoryDir := t.TempDir()
	pidFile := filepath.Join(memoryDir, ".pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	t.Setenv("PATH", t.TempDir())

	if err := Ensure(memoryDir, 6309); err == nil {
		t.Fatal("ensureManagedServer proceeded without knowing who owns the recorded pid")
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("pid file was cleared on an unknown probe: %v", err)
	}
}

// TestEnsure spawns the stub as the managed server, adopts it while current,
// restarts it when the recorded version is stale, and reports a spawn that
// cannot bind its port.
func TestEnsure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	provisiontest.InstallBinary(t, home, "demarkus-server", provisiontest.BuildStub(t))
	memory := filepath.Join(home, "memory")
	port := provisiontest.FreePort(t)
	pidFile := filepath.Join(memory, ".pid")
	versionFile := filepath.Join(memory, ".server-version")
	t.Cleanup(func() {
		if pid := procscan.ReadPID(pidFile); pid > 0 {
			_ = procscan.Signal(pid, syscall.SIGKILL)
		}
	})
	if err := Ensure(memory, port); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	pid := procscan.ReadPID(pidFile)
	if pid <= 0 {
		t.Fatal("no pid recorded after spawn")
	}
	if got, err := os.ReadFile(versionFile); err != nil || string(got) != release.ServerVersion+"\n" {
		t.Fatalf("version stamp = %q, %v; want the pin", got, err)
	}
	if procscan.PortIsFree(port) {
		t.Errorf("port %d is free after spawn", port)
	}

	if err := Ensure(memory, port); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if again := procscan.ReadPID(pidFile); again != pid {
		t.Errorf("a current server was respawned: pid %d -> %d", pid, again)
	}

	if err := os.WriteFile(versionFile, []byte("0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(memory, port); err != nil {
		t.Fatalf("Ensure over a stale version: %v", err)
	}
	fresh := procscan.ReadPID(pidFile)
	if fresh == pid || fresh <= 0 {
		t.Fatalf("stale server not restarted: pid %d -> %d", pid, fresh)
	}
	if lockdir.PidAlive(pid) {
		t.Errorf("stale server pid %d still alive after restart", pid)
	}

	// A binary that dies at startup is reported with its log and leaves no
	// bookkeeping behind. Nothing reaps it here: Ensure must see the exit
	// itself, since kill 0 reports an unreaped child as alive.
	provisiontest.WriteBinary(t, home, "demarkus-server", []byte("#!/bin/sh\necho boom >&2\nexit 1\n"))
	other := filepath.Join(home, "other")
	err := Ensure(other, provisiontest.FreePort(t))
	if err == nil || !strings.Contains(err.Error(), "failed to start") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Ensure with a dying binary = %v, want a start failure quoting the log", err)
	}
	if _, err := os.Stat(filepath.Join(other, ".pid")); !os.IsNotExist(err) {
		t.Errorf("pid file left behind after a failed spawn: %v", err)
	}
}
