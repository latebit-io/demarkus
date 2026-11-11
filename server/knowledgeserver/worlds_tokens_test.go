package knowledgeserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/internal/configwatch"
)

// eventually polls check until it holds or the deadline passes.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A watcher whose directory is not there yet must not give up for the life
// of the process: once the directory appears it reloads, and keeps watching.
func TestSuperviseWatcherStartsOnceItsDirectoryAppears(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-mounted-yet")
	target := filepath.Join(dir, "tokens.toml")
	var reloads atomic.Int64
	watcher := &configwatch.Watcher{
		Targets:  []string{target},
		Reload:   func() error { reloads.Add(1); return nil },
		Logger:   slog.New(slog.DiscardHandler),
		Debounce: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseWatcher(ctx, watcher, slog.New(slog.DiscardHandler), 20*time.Millisecond)
	}()

	// It fails to start a few times, reloading nothing.
	time.Sleep(100 * time.Millisecond)
	if got := reloads.Load(); got != 0 {
		t.Fatalf("reloads before the directory exists = %d", got)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the reload a late start owes", func() bool { return reloads.Load() >= 1 })

	// And it watches from then on: a write to the target reloads again.
	seen := reloads.Load()
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the reload after a change", func() bool { return reloads.Load() > seen })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor outlived its context")
	}
}
