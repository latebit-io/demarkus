// Package server runs the managed demarkus-server: spawn, adopt when current,
// stop and respawn when stale. A recorded .pid is only ever killed once proven
// to be our server for this root.
package server

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/release"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/tokens"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/progress"
)

// managedServerCurrent reports whether the managed server recorded in pidFile is
// alive, is genuinely OUR demarkus-server for root (not a reused PID), AND the
// binary version stamped in versionFile matches the current server pin.
func managedServerCurrent(pidFile, versionFile, root string) (bool, error) {
	switch procscan.ProbeAtRoot(procscan.ReadPID(pidFile), root) {
	case procscan.Unknown:
		return false, procscan.ErrOwnershipUnknown
	case procscan.NotOurs:
		return false, nil
	}
	// A missing stamp reads as not current, which restarts onto the pinned binary.
	ver, err := os.ReadFile(versionFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", versionFile, err)
	}
	return strings.TrimSpace(string(ver)) == release.ServerVersion, nil
}

// stopStaleManagedServer stops a live-but-stale managed server and confirms it
// exited: the caller's token migration moves the file the old server reads. A
// live PID that is not our server for memoryDir is left untouched (kill safety).
func stopStaleManagedServer(runningPID int, memoryDir string) error {
	switch procscan.ProbeAtRoot(runningPID, memoryDir) {
	case procscan.Unknown:
		return procscan.ErrOwnershipUnknown
	case procscan.NotOurs:
		if runningPID > 0 && lockdir.PidAlive(runningPID) {
			progress.Warnf("recorded pid %d is live but is not the demarkus-server for %s (stale .pid, reused PID); leaving it alone and clearing our bookkeeping", runningPID, memoryDir)
		}
		return nil
	}
	progress.Logf("restarting managed demarkus-server onto upgraded binary (target=%s)", release.ServerVersion)
	if err := procscan.Signal(runningPID, syscall.SIGTERM); err != nil && !procscan.AlreadyExited(err) {
		progress.Warnf("SIGTERM to managed server pid %d: %v", runningPID, err)
	}
	// Bounded wait for exit (~3s), then escalate to SIGKILL so the respawn
	// can bind the freed UDP port.
	for waited := 0; waited < 30 && lockdir.PidAlive(runningPID); waited++ {
		time.Sleep(100 * time.Millisecond)
	}
	if lockdir.PidAlive(runningPID) {
		if err := procscan.Signal(runningPID, syscall.SIGKILL); err != nil && !procscan.AlreadyExited(err) {
			progress.Warnf("SIGKILL to managed server pid %d: %v", runningPID, err)
		}
		for waited := 0; waited < 10 && lockdir.PidAlive(runningPID); waited++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if lockdir.PidAlive(runningPID) {
		return fmt.Errorf("prior managed server (pid %d) still alive after SIGKILL; aborting respawn", runningPID)
	}
	return nil
}

// retireStaleServer reports a current managed server, or stops a stale one of
// ours and clears its bookkeeping. An unknown probe aborts before touching either.
func retireStaleServer(pidFile, versionFile, memoryDir string) (bool, error) {
	current, err := managedServerCurrent(pidFile, versionFile, memoryDir)
	if err != nil || current {
		return current, err
	}
	if err := stopStaleManagedServer(procscan.ReadPID(pidFile), memoryDir); err != nil {
		return false, err
	}
	for _, f := range []string{pidFile, versionFile} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			progress.Warnf("could not clear stale bookkeeping file %s: %v", f, err)
		}
	}
	return false, nil
}

// Ensure spawns a demarkus-server for memoryDir unless ours is
// already running and current (default/isolated modes only). A recorded PID is
// killed only once confirmed to be our server for this root; a stale PID is left alone.
func Ensure(memoryDir string, port int) error {
	pidFile := filepath.Join(memoryDir, ".pid")
	versionFile := filepath.Join(memoryDir, ".server-version")

	current, err := retireStaleServer(pidFile, versionFile, memoryDir)
	if err != nil || current {
		return err
	}
	// Pre-#289 the log sat at <memoryDir>/.log, inside the server's tokens-watch
	// directory, feeding the watcher its own output. Remove it on migration.
	legacyLog := filepath.Join(memoryDir, ".log")
	if err := os.Remove(legacyLog); err != nil && !os.IsNotExist(err) {
		progress.Warnf("remove legacy server log %s: %v", legacyLog, err)
	}

	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		return err
	}
	tokensPath, err := tokens.MigrateManaged(memoryDir)
	if err != nil {
		return err
	}

	serverBin, err := config.BinPath("demarkus-server")
	if err != nil {
		return err
	}
	logFile, err := config.ManagedServerLogPath(memoryDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()

	cmd := exec.Command(serverBin,
		"-root", memoryDir,
		"-port", strconv.Itoa(port),
		"-tokens", tokensPath,
	)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn demarkus-server: %w", err)
	}
	pid := cmd.Process.Pid
	// If we can't record ownership bookkeeping, kill the just-spawned server
	// rather than leave a detached process with no valid .pid/.server-version
	// (which a later run can neither recognize nor safely manage).
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(pidFile)
		return err
	}
	if err := os.WriteFile(versionFile, []byte(release.ServerVersion+"\n"), 0o644); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(pidFile)
		_ = os.Remove(versionFile)
		return err
	}
	return (&spawned{cmd: cmd, pidFile: pidFile, versionFile: versionFile, logFile: logFile, port: port}).awaitReady(memoryDir)
}

// spawned is a just started managed server and the bookkeeping written for it.
type spawned struct {
	cmd                           *exec.Cmd
	pidFile, versionFile, logFile string
	port                          int
}

// awaitReady polls until the server binds its port or dies. Reaped by this
// process while it lives: kill 0 reports an unreaped child as alive, so
// without Wait a server that died at startup would read as spawned.
func (s *spawned) awaitReady(memoryDir string) error {
	pid := s.cmd.Process.Pid
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		if err := s.cmd.Wait(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				progress.Warnf("wait for demarkus-server (pid=%d): %v", pid, err)
			}
		}
	}()
	hasExited := func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}

	// Bounded poll: fail fast if the process died (bind/startup error), succeed
	// once the port is observed bound, or accept once the attempt cap is reached
	// with the process still alive (permissive when the port probe is unavailable).
	const maxAttempts = 20 // ~2s at 100ms
	for range maxAttempts {
		if hasExited() {
			return s.startupFailure()
		}
		if !procscan.PortIsFree(s.port) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// An exit during the last sleep must not read as a spawn.
	if hasExited() {
		return s.startupFailure()
	}
	progress.Logf("spawned demarkus-server (pid=%d, port=%d, root=%s)", pid, s.port, memoryDir)
	return nil
}

// startupFailure clears the bookkeeping of a server that died before it bound
// its port and names the port with the log tail.
func (s *spawned) startupFailure() error {
	for _, f := range []string{s.pidFile, s.versionFile} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			progress.Warnf("could not clear stale bookkeeping file %s: %v", f, err)
		}
	}
	tailInfo := ""
	if t := tailFile(s.logFile, 5); t != "" {
		tailInfo = "\nrecent log:\n" + t
	}
	return fmt.Errorf("demarkus-server failed to start (port %d may be in use; re-run /soul-init)%s", s.port, tailInfo)
}

// tailFile returns the last n lines of a file, or "" on error.
func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
