// Package configwatch invokes a reload callback when a watched file changes
// on disk. It watches the file's parent directory so it tolerates atomic
// rename swaps and symlink retargets, where the target inode changes under a
// stable path. Events are coalesced through a debounce window so a burst of
// writes triggers one reload.
//
// Platform caveat: macOS kqueue diffs directory entries by name, so a
// same-name atomic symlink swap within one rescan window emits no event;
// Linux inotify (production) reports renames explicitly.
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

// Watcher invokes Reload whenever the directory holding Targets sees any
// change, after coalescing events through Debounce.
type Watcher struct {
	// Targets are files in one directory; a write to any of them reloads,
	// as does any structural change in that directory.
	Targets  []string
	Reload   func() error
	Debounce time.Duration
	Logger   *slog.Logger
}

// Run blocks until ctx is canceled. It is intended to run in its own
// goroutine. Reload errors are logged and do not terminate the loop; only a
// failure to set up the underlying watcher returns an error.
func (w *Watcher) Run(ctx context.Context) error {
	if len(w.Targets) == 0 || w.Targets[0] == "" {
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

	dir := filepath.Dir(w.Targets[0])
	targets := make(map[string]bool, len(w.Targets))
	for _, target := range w.Targets {
		if filepath.Dir(target) != dir {
			return fmt.Errorf("configwatch: target %s is outside %s", target, dir)
		}
		targets[filepath.Clean(target)] = true
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("configwatch: create watcher: %w", err)
	}
	defer func() {
		if err := fw.Close(); err != nil {
			w.Logger.Warn("configwatch: close watcher failed", "target", w.Targets[0], "error", err)
		}
	}()

	if err := fw.Add(dir); err != nil {
		return fmt.Errorf("configwatch: watch %s: %w", dir, err)
	}

	w.Logger.Info("configwatch: watching", "targets", w.Targets, "dir", dir, "debounce", debounce)

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
			w.Logger.Warn("configwatch: error", "target", w.Targets[0], "error", err)
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			if !eventRelevant(ev, targets) {
				continue
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
				w.Logger.Warn("configwatch: reload failed", "target", w.Targets[0], "error", err)
			} else {
				w.Logger.Info("configwatch: reloaded", "target", w.Targets[0])
			}
		}
	}
}

// eventRelevant drops Write/Chmod events on sibling files (a co-located log
// once fed the watcher its own "reloaded" lines forever, #289). The targets
// themselves and structural ops (create/rename/remove: swaps) still reload.
func eventRelevant(ev fsnotify.Event, targets map[string]bool) bool {
	if ev.Name == "" || targets[filepath.Clean(ev.Name)] {
		return true
	}
	return ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0
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
