package lockdir

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
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
}
