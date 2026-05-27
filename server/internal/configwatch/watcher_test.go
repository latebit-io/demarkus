package configwatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runInBackground starts w.Run on a goroutine and registers a cleanup that
// cancels the context and asserts Run returned without error within 1s. This
// keeps Run's error surfaced to the test rather than discarded by the
// goroutine, and ensures the watcher goroutine is joined before the test
// returns.
func runInBackground(t *testing.T, w *Watcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("Run() did not return within 1s after ctx cancel")
		}
	})
}

// waitForCount blocks until the counter reaches at least want or the deadline
// passes. Returns the final value.
func waitForCount(counter *atomic.Int32, want int32, deadline time.Duration) int32 {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if counter.Load() >= want {
			return counter.Load()
		}
		time.Sleep(5 * time.Millisecond)
	}
	return counter.Load()
}

func TestRunValidatesInputs(t *testing.T) {
	tests := []struct {
		name string
		w    Watcher
		want string
	}{
		{"empty target", Watcher{Reload: func() error { return nil }, Logger: discardLogger()}, "target is empty"},
		{"nil reload", Watcher{Target: "/tmp/x", Logger: discardLogger()}, "reload callback is nil"},
		{"nil logger", Watcher{Target: "/tmp/x", Reload: func() error { return nil }}, "logger is nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.w.Run(t.Context())
			if err == nil || !contains(err.Error(), tt.want) {
				t.Fatalf("Run() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestInPlaceWriteTriggersReload(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tokens.toml")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	w := Watcher{
		Target:   target,
		Reload:   func() error { calls.Add(1); return nil },
		Debounce: 20 * time.Millisecond,
		Logger:   discardLogger(),
	}
	runInBackground(t, &w)

	time.Sleep(50 * time.Millisecond) // let watcher attach
	if err := os.WriteFile(target, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := waitForCount(&calls, 1, time.Second); got < 1 {
		t.Fatalf("expected at least 1 reload call, got %d", got)
	}
}

func TestAtomicRenameTriggersReload(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tokens.toml")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	w := Watcher{
		Target:   target,
		Reload:   func() error { calls.Add(1); return nil },
		Debounce: 20 * time.Millisecond,
		Logger:   discardLogger(),
	}
	runInBackground(t, &w)

	time.Sleep(50 * time.Millisecond)
	tmp := filepath.Join(dir, "tokens.toml.tmp")
	if err := os.WriteFile(tmp, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, target); err != nil {
		t.Fatal(err)
	}

	if got := waitForCount(&calls, 1, time.Second); got < 1 {
		t.Fatalf("expected at least 1 reload call after atomic rename, got %d", got)
	}
}

// TestSymlinkRetargetTriggersReload simulates the canonical mount-refresh
// pattern: the target path is a symlink that points through a stable
// indirection (here "current") into one of several content directories.
// Swapping the indirection atomically retargets every leaf symlink at once,
// the same way a process refreshing a mounted config tree does.
func TestSymlinkRetargetTriggersReload(t *testing.T) {
	dir := t.TempDir()

	v1 := filepath.Join(dir, "v1")
	v2 := filepath.Join(dir, "v2")
	for _, d := range []string{v1, v2} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(v1, "tokens.toml"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2, "tokens.toml"), []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}

	current := filepath.Join(dir, "current")
	if err := os.Symlink(v1, current); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "tokens.toml")
	if err := os.Symlink(filepath.Join(current, "tokens.toml"), target); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	w := Watcher{
		Target:   target,
		Reload:   func() error { calls.Add(1); return nil },
		Debounce: 20 * time.Millisecond,
		Logger:   discardLogger(),
	}
	runInBackground(t, &w)

	time.Sleep(50 * time.Millisecond)
	// Atomic swap of the indirection: stage a new symlink and rename it over
	// the old one. os.Rename replaces the destination atomically on POSIX.
	staged := filepath.Join(dir, "current.new")
	if err := os.Symlink(v2, staged); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, current); err != nil {
		t.Fatal(err)
	}

	if got := waitForCount(&calls, 1, time.Second); got < 1 {
		t.Fatalf("expected at least 1 reload call after symlink retarget, got %d", got)
	}
}

func TestDebounceCoalescesBurst(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tokens.toml")
	if err := os.WriteFile(target, []byte("v0"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	w := Watcher{
		Target:   target,
		Reload:   func() error { calls.Add(1); return nil },
		Debounce: 80 * time.Millisecond,
		Logger:   discardLogger(),
	}
	runInBackground(t, &w)

	time.Sleep(50 * time.Millisecond)
	// Fire several writes well within the debounce window.
	for i := range 10 {
		if err := os.WriteFile(target, []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Let the debounce window elapse and the reload fire.
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 reload from coalesced burst, got %d", got)
	}
}

func TestCtxCancelReturnsCleanly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "tokens.toml")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := Watcher{
		Target:   target,
		Reload:   func() error { return nil },
		Debounce: 20 * time.Millisecond,
		Logger:   discardLogger(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned error after ctx cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return within 1s after ctx cancel")
	}
}

func TestReloadWithRetryENOENT(t *testing.T) {
	var calls atomic.Int32
	reload := func() error {
		n := calls.Add(1)
		if n == 1 {
			return os.ErrNotExist
		}
		return nil
	}
	if err := reloadWithRetry(reload); err != nil {
		t.Fatalf("reloadWithRetry returned %v, want nil after retry", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 calls (initial + retry), got %d", got)
	}
}

func TestReloadWithRetryNonTransientErrorPropagates(t *testing.T) {
	sentinel := errors.New("parse error")
	var calls atomic.Int32
	reload := func() error {
		calls.Add(1)
		return sentinel
	}
	err := reloadWithRetry(reload)
	if !errors.Is(err, sentinel) {
		t.Fatalf("reloadWithRetry returned %v, want %v", err, sentinel)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 call (no retry for non-transient), got %d", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
