package lockdir

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestWithLock(t *testing.T) {
	t.Run("serializes concurrent holders", func(t *testing.T) {
		lock := filepath.Join(t.TempDir(), "state.lock")
		var mu sync.Mutex
		var inside, maxInside int

		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				err := WithLock(lock, 500, time.Millisecond, func() error {
					mu.Lock()
					inside++
					if inside > maxInside {
						maxInside = inside
					}
					mu.Unlock()
					time.Sleep(2 * time.Millisecond)
					mu.Lock()
					inside--
					mu.Unlock()
					return nil
				})
				if err != nil {
					t.Errorf("WithLock: %v", err)
				}
			})
		}
		wg.Wait()
		if maxInside != 1 {
			t.Errorf("critical section overlap: max %d holders", maxInside)
		}
	})

	t.Run("reclaims stale lock of dead owner", func(t *testing.T) {
		lock := filepath.Join(t.TempDir(), "state.lock")
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		// PID 1 is never signalable as ours on darwin/linux CI... use an
		// impossible-but-parseable dead pid instead: a huge value FindProcess
		// accepts and Signal(0) rejects.
		if err := os.WriteFile(filepath.Join(lock, "pid"), []byte("99999999"), 0o644); err != nil {
			t.Fatal(err)
		}
		ran := false
		if err := WithLock(lock, 100, time.Millisecond, func() error { ran = true; return nil }); err != nil {
			t.Fatalf("WithLock: %v", err)
		}
		if !ran {
			t.Error("fn did not run after stale reclaim")
		}
	})

	t.Run("reclaims lock dir with no pid stamp", func(t *testing.T) {
		// Legacy pre-staging crash shape: lock dir exists, never stamped.
		lock := filepath.Join(t.TempDir(), "state.lock")
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lock, "marker"), []byte("x"), 0o644); err != nil {
			t.Fatal(err) // non-empty so the staged rename can't silently replace it
		}
		ran := false
		if err := WithLock(lock, 100, time.Millisecond, func() error { ran = true; return nil }); err != nil {
			t.Fatalf("WithLock: %v", err)
		}
		if !ran {
			t.Error("fn did not run after unstamped-lock reclaim")
		}
	})

	t.Run("acquires over empty legacy lock dir", func(t *testing.T) {
		// Pre-staging crash between mkdir and stamp left an EMPTY dir.
		// Mixed-version writers never overlap (see package doc), so this is
		// always garbage; acquisition must succeed, not time out.
		lock := filepath.Join(t.TempDir(), "state.lock")
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		ran := false
		if err := WithLock(lock, 100, time.Millisecond, func() error { ran = true; return nil }); err != nil {
			t.Fatalf("WithLock: %v", err)
		}
		if !ran {
			t.Error("fn did not run over empty legacy lock dir")
		}
	})

	t.Run("reclaims lock with malformed pid stamp", func(t *testing.T) {
		lock := filepath.Join(t.TempDir(), "state.lock")
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lock, "pid"), []byte("not-a-pid"), 0o644); err != nil {
			t.Fatal(err)
		}
		ran := false
		if err := WithLock(lock, 100, time.Millisecond, func() error { ran = true; return nil }); err != nil {
			t.Fatalf("WithLock: %v", err)
		}
		if !ran {
			t.Error("fn did not run after corrupt-stamp reclaim")
		}
	})

	t.Run("times out on live holder", func(t *testing.T) {
		lock := filepath.Join(t.TempDir(), "state.lock")
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			t.Fatal(err)
		}
		err := WithLock(lock, 3, time.Millisecond, func() error {
			t.Error("fn must not run while a live holder owns the lock")
			return nil
		})
		if err == nil {
			t.Fatal("expected acquisition timeout")
		}
	})

	t.Run("times out on stuck transition guard", func(t *testing.T) {
		lock := filepath.Join(t.TempDir(), "state.lock")
		guard, err := os.OpenFile(lock+".guard", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lock, "pid"), []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
		guardClosed := false
		defer func() {
			if guardClosed {
				return
			}
			if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_UN); err != nil {
				t.Errorf("unlock guard: %v", err)
			}
			if err := guard.Close(); err != nil {
				t.Errorf("close guard: %v", err)
			}
		}()

		result := make(chan error, 1)
		go func() {
			result <- WithLock(lock, 100, time.Millisecond, func() error {
				t.Error("fn must not run while transition guard is held")
				return nil
			})
		}()

		time.Sleep(5 * time.Millisecond)
		if err := os.RemoveAll(lock); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		wantPID := strconv.Itoa(os.Getpid())
		if err := os.WriteFile(filepath.Join(lock, "pid"), []byte(wantPID), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
		guardClosed = true
		var contenderErr error
		select {
		case contenderErr = <-result:
		case <-time.After(time.Second):
			t.Fatal("contender did not exhaust retries")
		}
		if contenderErr == nil {
			t.Fatal("expected transition guard timeout")
		}
		pid, err := os.ReadFile(filepath.Join(lock, "pid"))
		if err != nil || string(pid) != wantPID {
			t.Fatalf("live holder was disturbed: pid=%q err=%v", pid, err)
		}
		stages, err := filepath.Glob(lock + ".stage-*")
		if err != nil {
			t.Fatal(err)
		}
		if len(stages) != 0 {
			t.Fatalf("contender leaked stages: %v", stages)
		}
		ran := false
		if err := WithLock(lock, 1, time.Millisecond, func() error { ran = true; return nil }); err == nil || ran {
			t.Fatalf("live holder was not preserved: err=%v ran=%v", err, ran)
		}
	})
}

// stampLock leaves a lock dir as a holder with pid, stamped at taken, left it.
func stampLock(t *testing.T, pid int, taken *time.Time) string {
	t.Helper()
	lock := filepath.Join(t.TempDir(), "state.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"pid": strconv.Itoa(pid)}
	if taken != nil {
		files[stampedFile] = strconv.FormatInt(taken.UnixNano(), 10)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(lock, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return lock
}

// A dead holder's PID can go to another process (a reboot does it readily);
// that process started after the lock was taken, so the lock is stale.
func TestWithLockReclaimsAReusedPID(t *testing.T) {
	if _, ok := ProcessStart(os.Getpid()); !ok {
		t.Skip("ps reports no start time here")
	}
	longAgo := time.Now().Add(-24 * time.Hour)
	ran := false
	if err := WithLock(stampLock(t, os.Getpid(), &longAgo), 20, time.Millisecond, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("a lock stamped before its live PID started was not reclaimed: ran=%v err=%v", ran, err)
	}
}

// A live holder is never stolen from: one whose stamp follows its start, or a
// legacy lock with no stamp time at all.
func TestWithLockWaitsOnALiveHolder(t *testing.T) {
	now := time.Now()
	for name, taken := range map[string]*time.Time{"stamped": &now, "legacy": nil} {
		lock := stampLock(t, os.Getpid(), taken)
		err := WithLock(lock, 5, time.Millisecond, func() error { t.Errorf("%s: ran inside a held lock", name); return nil })
		if err == nil {
			t.Errorf("%s: a live holder's lock was acquired", name)
		}
		if _, statErr := os.Stat(filepath.Join(lock, "pid")); statErr != nil {
			t.Errorf("%s: the held lock was disturbed: %v", name, statErr)
		}
	}
}

func TestAcquireStampsPIDAndTime(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "state.lock")
	before := time.Now()
	err := WithLock(lock, 5, time.Millisecond, func() error {
		pid, err := os.ReadFile(filepath.Join(lock, "pid"))
		if err != nil || string(pid) != strconv.Itoa(os.Getpid()) {
			t.Errorf("pid stamp = %q, %v (a bare number: older binaries read it)", pid, err)
		}
		stamp, err := os.ReadFile(filepath.Join(lock, stampedFile))
		nanos, convErr := strconv.ParseInt(string(stamp), 10, 64)
		if err != nil || convErr != nil || time.Unix(0, nanos).Before(before) {
			t.Errorf("time stamp = %q, %v %v", stamp, err, convErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
