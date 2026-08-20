// Package lockdir provides an atomic mkdir mutex with PID-stamped stale-lock
// recovery, shared by registry (state writes) and provision (session startup).
// The directory remains the held-lock marker, so no fd spans the critical
// section. A short-lived flock serializes acquire/reclaim/release transitions.
// Assumption: mixed-version writers never overlap. Legacy (pre-staging) locks
// are reclaimed as stale when unstamped or corrupt; a live legacy writer
// caught mid-stamp could be stolen from, but the plugin binary is replaced
// between short-lived invocations, so old and new writers cannot coexist.
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

var errGuardTimeout = errors.New("lock transition guard timed out")

const (
	guardAttempts = 100
	guardSleep    = time.Millisecond
)

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
				if err := reclaimStale(lockDir); err != nil {
					return err
				}
				time.Sleep(sleep)
				continue
			}
			if !PidAlive(pid) {
				if err := reclaimStale(lockDir); err != nil {
					return err
				}
				time.Sleep(sleep)
				continue
			}
		case errors.Is(readErr, os.ErrNotExist):
			exists, statErr := dirExists(lockDir)
			if statErr != nil {
				return statErr
			}
			if exists {
				if err := reclaimStale(lockDir); err != nil {
					return err
				}
				time.Sleep(sleep)
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
	var renameErr error
	renamed := false
	guardErr := withGuard(lockDir, 1, func() error {
		renameErr = os.Rename(stage, lockDir)
		renamed = renameErr == nil
		return nil
	})
	if guardErr != nil {
		if renamed {
			return false, errors.Join(guardErr, release(lockDir))
		}
		var acquireErr error
		if renameErr != nil {
			acquireErr = fmt.Errorf("acquire lock: %w", renameErr)
		}
		cleanupErr := os.RemoveAll(stage)
		if guardErr == errGuardTimeout {
			return false, cleanupErr
		}
		return false, errors.Join(guardErr, acquireErr, cleanupErr)
	}
	if renamed {
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

// reclaimStale revalidates ownership under the transition guard, then renames a
// dead or legacy lock aside. The live lock path is never moved speculatively.
func reclaimStale(lockDir string) error {
	aside := fmt.Sprintf("%s.stale.%d-%d", lockDir, os.Getpid(), nameSeq.Add(1))
	moved := false
	guardErr := withGuard(lockDir, 1, func() error {
		b, readErr := os.ReadFile(filepath.Join(lockDir, "pid"))
		if readErr == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr == nil && PidAlive(pid) {
				return nil
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("read lock pid before reclaim: %w", readErr)
		}
		if err := os.Rename(lockDir, aside); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("rename stale lock aside: %w", err)
		}
		moved = true
		return nil
	})
	if guardErr != nil {
		if moved {
			return errors.Join(guardErr, os.RemoveAll(aside))
		}
		if guardErr == errGuardTimeout {
			return nil
		}
		return guardErr
	}
	if moved {
		if err := os.RemoveAll(aside); err != nil {
			return fmt.Errorf("remove stale lock %s: %w", aside, err)
		}
	}
	return nil
}

// runLocked runs fn and releases the lock dir afterward, even if fn panics.
// A failed release joins the returned error; during panic unwinding it logs
// to stderr instead (the named return is dead, recover would hide it).
func runLocked(lockDir string, fn func() error) (err error) {
	completed := false
	defer func() {
		rmErr := release(lockDir)
		if rmErr == nil {
			return
		}
		if completed {
			err = errors.Join(err, fmt.Errorf("release %s: %w", lockDir, rmErr))
			return
		}
		fmt.Fprintf(os.Stderr, "lockdir: release %s during panic: %v\n", lockDir, rmErr)
	}()
	err = fn()
	completed = true
	return err
}

// release vacates the lock path atomically before recursive cleanup. Removing
// in place exposes a pid-less dir that another waiter can mistake for stale.
func release(lockDir string) error {
	aside := fmt.Sprintf("%s.release.%d-%d", lockDir, os.Getpid(), nameSeq.Add(1))
	moved := false
	guardErr := withGuard(lockDir, guardAttempts, func() error {
		if err := os.Rename(lockDir, aside); err != nil {
			return err
		}
		moved = true
		return nil
	})
	if guardErr != nil {
		if moved {
			return errors.Join(fmt.Errorf("rename lock aside: %w", guardErr), os.RemoveAll(aside))
		}
		return fmt.Errorf("rename lock aside: %w", guardErr)
	}
	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("remove released lock %s: %w", aside, err)
	}
	return nil
}

func withGuard(lockDir string, attempts int, fn func() error) (err error) {
	// This file is permanent: unlinking it can split flock users across inodes.
	guard, err := os.OpenFile(lockDir+".guard", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock transition guard: %w", err)
	}
	locked := false
	for attempt := range attempts {
		lockErr := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			locked = true
			break
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EAGAIN) {
			return errors.Join(fmt.Errorf("lock transition guard: %w", lockErr), guard.Close())
		}
		if attempt+1 < attempts {
			time.Sleep(guardSleep)
		}
	}
	if !locked {
		if closeErr := guard.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("%w after %s", errGuardTimeout, time.Duration(attempts)*guardSleep), closeErr)
		}
		return errGuardTimeout
	}
	defer func() {
		unlockErr := syscall.Flock(int(guard.Fd()), syscall.LOCK_UN)
		closeErr := guard.Close()
		if unlockErr != nil {
			unlockErr = fmt.Errorf("unlock transition guard: %w", unlockErr)
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("close transition guard: %w", closeErr)
		}
		err = errors.Join(err, unlockErr, closeErr)
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
