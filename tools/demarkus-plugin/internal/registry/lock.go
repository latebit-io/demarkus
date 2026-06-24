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
	for i := 0; i < 100; i++ {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			_ = os.WriteFile(lockPid, []byte(strconv.Itoa(os.Getpid())), 0o644)
			defer os.RemoveAll(lock)
			return fn()
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
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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
