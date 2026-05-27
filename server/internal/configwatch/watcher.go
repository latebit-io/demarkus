// Package configwatch invokes a reload callback when a watched file changes
// on disk. It watches the file's parent directory so it tolerates atomic
// rename swaps and symlink retargets, where the target inode changes under a
// stable path. Events are coalesced through a debounce window so a burst of
// writes triggers one reload.
package configwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultDebounce coalesces rapid-fire events from editors writing through a
// temp file or directory swaps that fan out into multiple kernel events.
const defaultDebounce = 150 * time.Millisecond

// retryDelay covers a brief window when a directory swap can leave a path
// momentarily unresolvable before the new contents are linked in.
const retryDelay = 75 * time.Millisecond

// Watcher invokes Reload whenever the directory holding Target sees any
// change, after coalescing events through Debounce.
type Watcher struct {
	Target   string
	Reload   func() error
	Debounce time.Duration
	Logger   *slog.Logger
}

// Run blocks until ctx is canceled. It is intended to run in its own
// goroutine. Reload errors are logged and do not terminate the loop; only a
// failure to set up the underlying watcher returns an error.
func (w *Watcher) Run(ctx context.Context) error {
	if w.Target == "" {
		return errors.New("configwatch: target is empty")
	}
	if w.Reload == nil {
		return errors.New("configwatch: reload callback is nil")
	}
	if w.Logger == nil {
		return errors.New("configwatch: logger is nil")
	}
	debounce := w.Debounce
	if debounce <= 0 {
		debounce = defaultDebounce
	}

	dir := filepath.Dir(w.Target)
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("configwatch: create watcher: %w", err)
	}
	defer func() { _ = fw.Close() }()

	if err := fw.Add(dir); err != nil {
		return fmt.Errorf("configwatch: watch %s: %w", dir, err)
	}

	w.Logger.Info("configwatch: watching", "target", w.Target, "dir", dir, "debounce", debounce)

	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.Logger.Warn("configwatch: error", "target", w.Target, "error", err)
		case _, ok := <-fw.Events:
			if !ok {
				return nil
			}
			if armed && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(debounce)
			armed = true
		case <-timer.C:
			armed = false
			if err := reloadWithRetry(w.Reload); err != nil {
				w.Logger.Warn("configwatch: reload failed", "target", w.Target, "error", err)
			} else {
				w.Logger.Info("configwatch: reloaded", "target", w.Target)
			}
		}
	}
}

// reloadWithRetry calls reload and retries once on a transient ErrNotExist,
// which can briefly surface when a directory swap removes the old path before
// the new one resolves.
func reloadWithRetry(reload func() error) error {
	err := reload()
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	time.Sleep(retryDelay)
	return reload()
}
