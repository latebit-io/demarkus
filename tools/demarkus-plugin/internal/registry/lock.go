// Package registry owns every mutation of the shared ~/.demarkus + ~/.config/mcp
// state (soul catalog, project bindings, knowledge registry, policy mirrors,
// promote targets, MCP server config). Centralizing it here replaces the
// per-plugin bash (soul-join.sh, soul-default.sh, register-knowledge.sh,
// mirror-policy.sh, promote-target.sh) and the mcp-config.mjs JS — one
// implementation, called by every harness's thin adapter.
package registry

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// withLock runs fn while holding an atomic mkdir mutex for path+".lock". The
// holder records its PID in the lock dir so a lock whose owner has died (crashed
// mid-write) is reclaimed instead of wedging future writers. Bounded ~2s.
func withLock(path string, fn func() error) error {
	lock := path + ".lock"
	lockPid := filepath.Join(lock, "pid")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	for range 100 {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			_ = os.WriteFile(lockPid, []byte(strconv.Itoa(os.Getpid())), 0o644)
			return runLocked(lock, fn)
		}
		if !os.IsExist(err) {
			return err
		}
		// stale-lock recovery: reclaim if the recorded owner is gone
		if owner, e := os.ReadFile(lockPid); e == nil {
			if pid, e2 := strconv.Atoi(string(trimSpace(owner))); e2 == nil && !pidAlive(pid) {
				_ = os.RemoveAll(lock)
				continue
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &lockError{lock}
}

// runLocked runs fn and releases the lock dir afterward, even if fn panics.
// Split out of withLock's loop so the cleanup defer isn't registered per
// iteration (it fires exactly once, on the acquiring iteration's return).
func runLocked(lock string, fn func() error) error {
	defer func() { _ = os.RemoveAll(lock) }()
	return fn()
}

type lockError struct{ lock string }

func (e *lockError) Error() string {
	return "could not acquire " + e.lock + " (held by another process?)"
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}

// atomicWrite writes data to path via a temp file + rename (no torn writes).
func atomicWrite(path string, data []byte) error { return atomicWritePerm(path, data, 0o644) }

// atomicWritePerm is atomicWrite with an explicit mode. The temp file is created
// WITH that mode (via O_CREATE perm) so a secret (e.g. a 0600 token) is never
// briefly world-readable before a later chmod.
func atomicWritePerm(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// O_CREATE perm is masked by umask; enforce the exact mode for secrets.
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}
