package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/certsource"
	"github.com/latebit-io/demarkus/server/internal/configwatch"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
)

// worldAddTimeout bounds one world's open (GCS dial, genesis, head load).
const worldAddTimeout = time.Minute

// worldRetryInterval paces re-attempts for worlds whose open failed
// (missing tokens file still propagating, transient GCS errors).
const worldRetryInterval = 15 * time.Second

// worldManager owns the live world set: it opens/closes/replaces worlds
// to match desired config and republishes the router and token views.
// The hot-reload seam for dynamic tenants (memory-broker plan Phase 3).
type worldManager struct {
	configFile string
	newStore   func(ctx context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error)
	logger     *slog.Logger
	certs      *certsource.Source
	router     *snirouter.Dynamic
	tokens     *tokenCoordinator

	watchCtx context.Context
	group    *sync.WaitGroup

	mu      sync.Mutex
	entries map[string]*worldEntry
	// tokenDirs refcounts one watcher per distinct tokens-file parent
	// dir: the coordinator reload is global, so per-world watchers on a
	// shared dynamic-tenant Secret dir would only multiply fsnotify load.
	tokenDirs map[string]*dirWatch
	// pending holds desired worlds whose open failed; the retry loop
	// re-attempts them until they come up or the config drops them.
	pending map[string]knowledgeconfig.WorldConfig
	// needsPublish marks a failed router/token publish so the retry
	// loop re-attempts it without waiting for another config event.
	needsPublish bool
	// resilient marks dynamic deployments (worldsFile set): a failed
	// world open degrades to pending instead of failing the process.
	resilient bool
}

type worldEntry struct {
	config  knowledgeconfig.WorldConfig
	runtime *worldruntime.Runtime
}

// newWorldManager opens the initial world set. In static mode (no
// worldsFile) any open failure is fatal, preserving the pre-dynamic
// startup contract; in dynamic mode failures go to pending.
func newWorldManager(
	watchCtx context.Context,
	group *sync.WaitGroup,
	configFile string,
	config *knowledgeconfig.Config,
	newStore func(ctx context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error),
	certs *certsource.Source,
	logger *slog.Logger,
) (*worldManager, error) {
	m := &worldManager{
		configFile: configFile,
		newStore:   newStore,
		logger:     logger,
		certs:      certs,
		watchCtx:   watchCtx,
		group:      group,
		entries:    make(map[string]*worldEntry),
		tokenDirs:  make(map[string]*dirWatch),
		pending:    make(map[string]knowledgeconfig.WorldConfig),
		resilient:  config.WorldsFile != "",
	}
	router, err := snirouter.NewDynamic(nil)
	if err != nil {
		return nil, err
	}
	m.router = router
	tokens, err := newTokenCoordinator(nil)
	if err != nil {
		return nil, err
	}
	m.tokens = tokens
	if err := m.apply(config.Worlds); err != nil {
		m.Close()
		return nil, err
	}
	group.Go(func() { m.retryLoop(watchCtx) })
	return m, nil
}

// Reload re-reads the merged configuration and applies the world set.
// Listener, TLS, and health changes require a restart and are logged.
func (m *worldManager) Reload() error {
	config, err := knowledgeconfig.Load(m.configFile)
	if err != nil {
		return err
	}
	return m.apply(config.Worlds)
}

// apply diffs desired against live. In resilient mode per-world failures
// are logged and retried; in static mode the first failure is returned.
func (m *worldManager) apply(desired []knowledgeconfig.WorldConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	want := make(map[string]*knowledgeconfig.WorldConfig, len(desired))
	for index := range desired {
		want[desired[index].Name] = &desired[index]
	}

	for name, entry := range m.entries {
		target, keep := want[name]
		if keep && reflect.DeepEqual(entry.config, *target) {
			continue
		}
		if keep {
			m.logger.Info("world config changed; replacing runtime", "world", name)
		} else {
			m.logger.Info("world removed", "world", name)
		}
		m.closeEntryLocked(name, entry)
	}
	for name := range m.pending {
		if _, keep := want[name]; !keep {
			delete(m.pending, name)
		}
	}

	var firstErr error
	for name, target := range want {
		if _, live := m.entries[name]; live {
			continue
		}
		if err := m.openLocked(target); err != nil {
			if !m.resilient {
				if firstErr == nil {
					firstErr = fmt.Errorf("open world %q: %w", name, err)
				}
				continue
			}
			m.logger.Warn("world open failed; will retry", "world", name, "err", err)
			m.pending[name] = *target
			continue
		}
		delete(m.pending, name)
	}
	// Publish unconditionally: the diff loop already closed removed and
	// replaced worlds, so returning early would keep routing to them.
	publishErr := m.publishLocked()
	if publishErr != nil && m.resilient {
		// Dynamic mode degrades like a failed open: log, mark for the
		// retry loop, keep the process alive.
		m.logger.Error("world publish failed; will retry", "error", publishErr)
		m.needsPublish = true
		publishErr = nil
	}
	return errors.Join(firstErr, publishErr)
}

// openLocked opens one world: blob store, optional genesis bootstrap,
// bucket store, runtime, and its token-file watcher.
func (m *worldManager) openLocked(world *knowledgeconfig.WorldConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), worldAddTimeout)
	defer cancel()

	objects, err := m.newStore(ctx, world)
	if err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	if world.Bootstrap {
		// Genesis only; no policy document. The provisioner (memory
		// broker) seeds policy through the protocol, so the store opens
		// without requiring one.
		if err := bucketstore.Initialize(ctx, objects, world.Bucket.WorldID); err != nil {
			return fmt.Errorf("bootstrap genesis: %w", err)
		}
	}
	store, err := bucketstore.Open(ctx, objects, bucketstore.Options{
		WorldID:        world.Bucket.WorldID,
		RequestTimeout: time.Duration(world.Limits.RequestTimeout),
		RequirePolicy:  !world.Bootstrap,
		MaxDocuments:   world.Limits.MaxDocuments,
	})
	if err != nil {
		return fmt.Errorf("bucket: %w", err)
	}
	runtime, err := worldruntime.New(&worldruntime.Config{
		Name:              world.Name,
		Store:             store,
		Catalog:           store,
		Views:             store,
		TokensFile:        world.Auth.TokensFile,
		DisableTokenWatch: true, // the coordinator owns reloads
		ReadOnly:          world.ReadOnly,
		RequestTimeout:    time.Duration(world.Limits.RequestTimeout),
		MaxConcurrent:     world.Limits.MaxConcurrentRequests,
		RateLimit:         world.Limits.RequestsPerSecond,
		RateBurst:         world.Limits.Burst,
		Logger:            m.logger,
	})
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	for _, authority := range world.Authorities {
		if err := m.certs.Covers(authority); err != nil {
			m.logger.Warn("TLS certificate does not cover world authority; verifying clients will fail until the cert rotates",
				"world", world.Name, "authority", authority, "err", err)
		}
	}
	entry := &worldEntry{config: *world, runtime: runtime}
	m.entries[world.Name] = entry
	m.acquireTokenWatchLocked(world.Auth.TokensFile)
	m.logger.Info("world opened", "world", world.Name, "bootstrap", world.Bootstrap)
	return nil
}

// dirWatch is one refcounted configwatch watcher covering every world
// whose tokens file lives in the same directory.
type dirWatch struct {
	count  int
	cancel context.CancelFunc
}

// acquireTokenWatchLocked starts (or shares) the watcher for the tokens
// file's parent directory. The reload is the global coordinator pass, so
// one watcher per directory is exactly as fresh as one per world.
func (m *worldManager) acquireTokenWatchLocked(tokensFile string) {
	dir := filepath.Dir(tokensFile)
	if watch, ok := m.tokenDirs[dir]; ok {
		watch.count++
		return
	}
	watchCtx, cancel := context.WithCancel(m.watchCtx)
	m.tokenDirs[dir] = &dirWatch{count: 1, cancel: cancel}
	watcher := &configwatch.Watcher{Target: tokensFile, Reload: m.tokens.Reload, Logger: m.logger}
	m.group.Go(func() {
		if err := watcher.Run(watchCtx); err != nil {
			m.logger.Warn("token watcher exited", "target", watcher.Target, "error", err)
		}
	})
}

func (m *worldManager) releaseTokenWatchLocked(tokensFile string) {
	dir := filepath.Dir(tokensFile)
	watch, ok := m.tokenDirs[dir]
	if !ok {
		return
	}
	watch.count--
	if watch.count > 0 {
		return
	}
	// No join: waiting under m.mu would stall every reload behind a
	// slow watcher exit; the shared group collects the goroutine and a
	// briefly overlapping successor only adds an idempotent reload.
	watch.cancel()
	delete(m.tokenDirs, dir)
}

func (m *worldManager) closeEntryLocked(name string, entry *worldEntry) {
	m.releaseTokenWatchLocked(entry.config.Auth.TokensFile)
	if err := entry.runtime.Close(); err != nil {
		m.logger.Warn("world runtime close failed", "world", name, "error", err)
	}
	delete(m.entries, name)
}

// publishLocked rebuilds the router and token-coordinator views from
// the live entries. A failure keeps that surface's previous view and is
// returned to the caller; every apply re-attempts, so it self-heals.
func (m *worldManager) publishLocked() error {
	mappings := make([]snirouter.Mapping, 0, len(m.entries))
	worlds := make([]tokenWorld, 0, len(m.entries))
	for name, entry := range m.entries {
		for _, authority := range entry.config.Authorities {
			mappings = append(mappings, snirouter.Mapping{Authority: authority, Endpoint: entry.runtime.Endpoint(authority)})
		}
		worlds = append(worlds, tokenWorld{name: name, runtime: entry.runtime})
	}
	// Prevalidate BOTH candidate views before committing either, so a
	// failure never leaves routing and token state referencing
	// different world sets.
	router, routerErr := snirouter.New(mappings)
	stores := make([]*auth.TokenStore, len(worlds))
	for index := range worlds {
		stores[index] = worlds[index].runtime.Tokens()
	}
	tokensErr := validateUniqueTokens(worlds, stores)
	if routerErr != nil || tokensErr != nil {
		return errors.Join(routerErr, tokensErr)
	}
	previous := m.router.Load()
	m.router.Store(router)
	if err := m.tokens.SetWorlds(worlds); err != nil {
		// A token reload raced between validate and commit; restore the
		// previous routing so the two views stay paired.
		m.router.Store(previous)
		return fmt.Errorf("token coordinator swap failed; rolled back routing: %w", err)
	}
	return nil
}

// retryLoop re-attempts pending world opens until ctx ends.
func (m *worldManager) retryLoop(ctx context.Context) {
	ticker := time.NewTicker(worldRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.retryPending()
		}
	}
}

func (m *worldManager) retryPending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 && !m.needsPublish {
		return
	}
	recovered := m.needsPublish
	for name := range m.pending {
		world := m.pending[name]
		if err := m.openLocked(&world); err != nil {
			m.logger.Warn("world open retry failed", "world", name, "err", err)
			continue
		}
		delete(m.pending, name)
		recovered = true
	}
	if recovered {
		if err := m.publishLocked(); err != nil {
			m.logger.Error("publish after world retry failed", "error", err)
			m.needsPublish = true
			return
		}
		m.needsPublish = false
	}
}

// Router exposes the dynamic router (selector + handshake hook source).
func (m *worldManager) Router() *snirouter.Dynamic { return m.router }

// Tokens exposes the coordinator for the SIGHUP reload path.
func (m *worldManager) Tokens() *tokenCoordinator { return m.tokens }

// WorldCount returns the live world count (startup logging).
func (m *worldManager) WorldCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Close closes every live world through the one teardown path.
func (m *worldManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, entry := range m.entries {
		m.closeEntryLocked(name, entry)
	}
}
