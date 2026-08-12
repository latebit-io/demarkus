// Package lockdir provides an atomic mkdir mutex with PID-stamped stale-lock
// recovery, shared by registry (state writes) and provision (session startup).
// mkdir (not flock like protocol/token) is deliberate: it keeps the existing
// on-disk lock layout and needs no held fd across the exec'd critical section.
package lockdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// nameSeq makes reclaim aside-names unique across goroutines in one process;
// the PID alone is not enough when several writers race in-process.
var nameSeq atomic.Uint64

// WithLock runs fn while holding the mutex at lockDir. Acquisition stages a
// pre-stamped dir and renames it into place, so a lock dir is never visible
// without its PID stamp. attempts*sleep bounds the total wait.
func WithLock(lockDir string, attempts int, sleep time.Duration, fn func() error) error {
	lockPid := filepath.Join(lockDir, "pid")
	if err := os.MkdirAll(filepath.Dir(lockDir), 0o755); err != nil {
		return err
	}
	for range attempts {
		acquired, err := tryAcquire(lockDir)
		if err != nil {
			return err
		}
		if acquired {
			return runLocked(lockDir, fn)
		}
		// Reclaim when the recorded owner is dead, or when the stamp is
		// missing entirely (legacy pre-staging crash left an unstamped dir;
		// staged acquisition can no longer produce one).
		b, readErr := os.ReadFile(lockPid)
		switch {
		case readErr == nil:
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr != nil {
				// Corrupt stamp: staged acquisition only writes valid PIDs,
				// so reclaim like the unstamped-legacy case (self-heal)
				// rather than treating it as live contention.
				reclaimStale(lockDir)
				continue
			}
			if !PidAlive(pid) {
				reclaimStale(lockDir)
				continue
			}
		case errors.Is(readErr, os.ErrNotExist):
			exists, statErr := dirExists(lockDir)
			if statErr != nil {
				return statErr
			}
			if exists {
				reclaimStale(lockDir)
				continue
			}
		default:
			// Permission/IO/ENOTDIR: surface now, not as a timeout later.
			return fmt.Errorf("read lock pid %s: %w", lockPid, readErr)
		}
		time.Sleep(sleep)
	}
	return fmt.Errorf("could not acquire %s (held by another process?)", lockDir)
}

// tryAcquire stages a dir already containing the PID stamp and renames it
// into the lock location: no unstamped window, rename onto a stamped lock
// fails (contention). MkdirTemp naming makes leftover stages collision-free.
func tryAcquire(lockDir string) (bool, error) {
	stage, err := os.MkdirTemp(filepath.Dir(lockDir), filepath.Base(lockDir)+".stage-")
	if err != nil {
		return false, fmt.Errorf("stage lock: %w", err)
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return false, errors.Join(fmt.Errorf("stage lock perms: %w", err), os.RemoveAll(stage))
	}
	if err := os.WriteFile(filepath.Join(stage, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return false, errors.Join(fmt.Errorf("stamp lock pid: %w", err), os.RemoveAll(stage))
	}
	renameErr := os.Rename(stage, lockDir)
	if renameErr == nil {
		return true, nil
	}
	cleanupErr := os.RemoveAll(stage)
	if isContention(renameErr) {
		return false, cleanupErr
	}
	return false, errors.Join(fmt.Errorf("acquire lock: %w", renameErr), cleanupErr)
}

// isContention reports a rename failure that just means another process
// holds the lock (target exists and is non-empty).
func isContention(err error) bool {
	return errors.Is(err, os.ErrExist) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// reclaimStale removes a dead holder's lock, ownership-safe: rename aside
// (one racer wins), re-read the moved pid, confirm still dead, delete.
// Best-effort: any failure means a racer won; the retry loop recovers.
func reclaimStale(lockDir string) {
	aside := fmt.Sprintf("%s.stale.%d-%d", lockDir, os.Getpid(), nameSeq.Add(1))
	if os.Rename(lockDir, aside) != nil {
		return
	}
	if b, err := os.ReadFile(filepath.Join(aside, "pid")); err == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && PidAlive(pid) {
			// Moved a freshly-acquired live lock — put it back.
			_ = os.Rename(aside, lockDir)
			return
		}
	}
	_ = os.RemoveAll(aside)
}

// runLocked runs fn and releases the lock dir afterward, even if fn panics.
// A failed release is surfaced: a lingering lock dir would wedge the next
// writer until stale reclaim.
func runLocked(lockDir string, fn func() error) (err error) {
	defer func() {
		if rmErr := os.RemoveAll(lockDir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("release %s: %w", lockDir, rmErr))
		}
	}()
	return fn()
}

// dirExists distinguishes "not there" from a failing Stat; unexpected
// failures surface instead of degrading into an acquisition timeout.
func dirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	return info.IsDir(), nil
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
