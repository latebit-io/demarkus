// Package lockdir provides an atomic mkdir mutex with PID-stamped stale-lock
// recovery, shared by registry (state writes) and provision (session startup).
// mkdir (not flock like protocol/token) is deliberate: it keeps the existing
// on-disk lock layout and needs no held fd across the exec'd critical section.
package lockdir

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// WithLock runs fn while holding the mkdir mutex at lockDir. The holder stamps
// its PID so a lock whose owner died is reclaimed instead of wedging waiters.
// attempts*sleep bounds the total wait.
func WithLock(lockDir string, attempts int, sleep time.Duration, fn func() error) error {
	lockPid := filepath.Join(lockDir, "pid")
	if err := os.MkdirAll(filepath.Dir(lockDir), 0o755); err != nil {
		return err
	}
	for range attempts {
		if err := os.Mkdir(lockDir, 0o755); err == nil {
			// Stamp our PID before the critical section. A failed write leaves an
			// unowned lock stale-lock recovery can never reclaim, so release and fail.
			if werr := os.WriteFile(lockPid, []byte(strconv.Itoa(os.Getpid())), 0o644); werr != nil {
				_ = os.RemoveAll(lockDir)
				return fmt.Errorf("write lock pid: %w", werr)
			}
			return runLocked(lockDir, fn)
		} else if !os.IsExist(err) {
			return err
		}
		if b, e := os.ReadFile(lockPid); e == nil {
			if pid, e2 := strconv.Atoi(strings.TrimSpace(string(b))); e2 == nil && !PidAlive(pid) {
				reclaimStale(lockDir)
				continue
			}
		}
		time.Sleep(sleep)
	}
	return fmt.Errorf("could not acquire %s (held by another process?)", lockDir)
}

// reclaimStale removes a dead holder's lock, ownership-safe: rename the lock
// dir aside (one racer's rename wins), re-read the moved pid, and confirm it is
// still dead before deleting. A blind RemoveAll could nuke a lock another
// process freshly acquired between the caller's read and the delete.
func reclaimStale(lockDir string) {
	aside := lockDir + ".stale." + strconv.Itoa(os.Getpid())
	if os.Rename(lockDir, aside) != nil {
		return
	}
	if b, err := os.ReadFile(filepath.Join(aside, "pid")); err == nil {
		if pid, err2 := strconv.Atoi(strings.TrimSpace(string(b))); err2 == nil && PidAlive(pid) {
			// Moved a freshly-acquired live lock — put it back.
			_ = os.Rename(aside, lockDir)
			return
		}
	}
	_ = os.RemoveAll(aside)
}

// runLocked runs fn and releases the lock dir afterward, even if fn panics.
// Split out so the cleanup defer registers once, on the acquiring return.
func runLocked(lockDir string, fn func() error) error {
	defer func() { _ = os.RemoveAll(lockDir) }()
	return fn()
}

// PidAlive reports whether pid refers to a live process.
func PidAlive(pid int) bool {
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
