package knowledgeserver

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/configwatch"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
)

// dirWatch is one refcounted configwatch watcher covering every world
// whose tokens file lives in the same directory.
type dirWatch struct {
	cancel context.CancelFunc
	// files refcounts every token file in the directory across worlds; the
	// watcher consults it per event, so a world added later is covered.
	mu    sync.Mutex
	files map[string]int
}

func (watch *dirWatch) add(file string) {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	watch.files[filepath.Clean(file)]++
}

// remove drops file and reports whether the directory has none left.
func (watch *dirWatch) remove(file string) bool {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	clean := filepath.Clean(file)
	watch.files[clean]--
	if watch.files[clean] <= 0 {
		delete(watch.files, clean)
	}
	return len(watch.files) == 0
}

func (watch *dirWatch) has(cleanPath string) bool {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	return watch.files[cleanPath] > 0
}

// tokenFiles lists a world's configured token files.
func tokenFiles(world *knowledgeconfig.WorldConfig) []string {
	return auth.SourceConfig{TokensFile: world.Auth.TokensFile, StaticTokensFile: world.Auth.StaticTokensFile}.Files()
}

// acquireTokenWatchLocked starts (or shares) the watcher of each file's
// directory. The reload is the global coordinator pass, so one watcher per
// directory is exactly as fresh as one per world.
func (m *worldManager) acquireTokenWatchLocked(files []string) {
	for _, file := range files {
		dir := filepath.Dir(file)
		if watch, ok := m.tokenDirs[dir]; ok {
			watch.add(file)
			continue
		}
		watchCtx, cancel := context.WithCancel(m.watchCtx)
		watch := &dirWatch{cancel: cancel, files: make(map[string]int, len(files))}
		watch.add(file)
		m.tokenDirs[dir] = watch
		watcher := &configwatch.Watcher{Targets: []string{file}, Relevant: watch.has, Reload: m.tokens.Reload, Logger: m.logger}
		m.group.Go(func() { superviseWatcher(watchCtx, watcher, m.logger, worldRetryInterval) })
	}
}

func (m *worldManager) releaseTokenWatchLocked(files []string) {
	for _, file := range files {
		dir := filepath.Dir(file)
		watch, ok := m.tokenDirs[dir]
		if !ok || !watch.remove(file) {
			continue
		}
		// No join: waiting under m.mu would stall every reload behind a
		// slow watcher exit; the shared group collects the goroutine and a
		// briefly overlapping successor only adds an idempotent reload.
		watch.cancel()
		delete(m.tokenDirs, dir)
	}
}

// superviseWatcher runs watcher until ctx ends, restarting it after every
// exit: one that could not start (no directory yet, no inotify instance)
// would leave its files unwatched. Each start reloads once.
func superviseWatcher(ctx context.Context, watcher *configwatch.Watcher, logger *slog.Logger, retry time.Duration) {
	lastErr := ""
	for {
		err := watcher.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		// The same failure every retry is one line, not one per interval.
		if text := errText(err); text != lastErr {
			lastErr = text
			logger.Warn("watcher exited; restarting it", "targets", watcher.Targets, "error", text, "retry", retry)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "event stream closed"
	}
	return err.Error()
}
