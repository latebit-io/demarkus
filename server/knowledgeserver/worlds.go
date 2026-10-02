package knowledgeserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/certsource"
	"github.com/latebit-io/demarkus/server/internal/configwatch"
	"github.com/latebit-io/demarkus/server/internal/knowledge/knowledgeseed"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/peerhint"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// worldAddTimeout bounds one world's open (store, policy seed, runtime).
const worldAddTimeout = time.Minute

// worldRetryInterval paces re-attempts for worlds whose open failed
// (transient GCS errors, an unreadable policy file).
const worldRetryInterval = 15 * time.Second

// worldManager owns the live world set: it opens/closes/replaces worlds
// to match desired config and republishes the router and token views.
// The hot-reload seam for dynamic tenants (memory-broker plan Phase 3).
type worldManager struct {
	configFile string
	openStore  storeFactory
	// hint tells peer replicas a world committed a sequence; nil without peers.
	hint   func(worldID string, sequence int64)
	logger *slog.Logger
	// maxStreams is the listener's per connection stream limit; runtimes
	// cap watches under it.
	maxStreams int
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
	// retiring holds replaced and removed worlds until a publish succeeds.
	retiring []retiredWorld
	// beforeRetire is a test hook, called before a retired runtime closes.
	beforeRetire func(name string)
	// resilient marks dynamic deployments (worldsFile set): a failed
	// world open degrades to pending instead of failing the process.
	resilient bool
	// reloadMu orders load-then-apply, so a file reload racing a push can
	// never apply a pre-push world set after it; the push state lives under it.
	reloadMu sync.Mutex
	// dynamic is a private copy of the last accepted push, in force once
	// dynamicSet; an empty push is a push, not an absence.
	dynamic    []byte
	dynamicSet bool
}

type worldEntry struct {
	config  knowledgeconfig.WorldConfig
	runtime *worldruntime.Runtime
	// store is owned by runtime, which closes it.
	store worldStore
	// published is set once a router publish carried this runtime; until
	// then nothing routes to it and it can close at once.
	published bool
}

// worldStore is a world's store as the manager holds it: backend contract
// types only, so any store the factory opens can serve. A store several
// replicas share also implements backend.Follower.
type worldStore interface {
	backend.Store
	backend.ChangeSource
	io.Closer
}

// storeFactory opens a world's store, creating the world in empty storage
// unless it is read-only. The server's wiring supplies the only one.
type storeFactory func(ctx context.Context, world *knowledgeconfig.WorldConfig, hooks storeHooks) (worldStore, error)

// storeHooks is what the manager hands a world's store at open.
type storeHooks struct {
	logger *slog.Logger
	// committed runs after each of this replica's own commits with its
	// sequence; nil without peers.
	committed func(sequence int64)
}

// worldManagerConfig is what a world manager is built from.
type worldManagerConfig struct {
	configFile string
	config     *knowledgeconfig.Config
	openStore  storeFactory
	certs      *certsource.Source
	// peers carries commit hints to and from the other replicas.
	peers  *peerLinks
	logger *slog.Logger
}

// newWorldManager opens the initial world set. In static mode (no
// worldsFile) any open failure is fatal, preserving the pre-dynamic
// startup contract; in dynamic mode failures go to pending.
func newWorldManager(watchCtx context.Context, group *sync.WaitGroup, cfg worldManagerConfig) (*worldManager, error) {
	config := cfg.config
	m := &worldManager{
		configFile: cfg.configFile,
		openStore:  cfg.openStore,
		hint:       cfg.peers.hint,
		logger:     cfg.logger,
		maxStreams: int(config.Listen.MaxIncomingStreams),
		certs:      cfg.certs,
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
	cfg.peers.deliverTo(m.Hint)
	group.Go(func() { m.retryLoop(watchCtx) })
	return m, nil
}

// Reload re-reads the merged configuration and applies the world set. A
// fragment set through SetDynamicWorlds stands in for the worldsFile.
// Listener, TLS, and health changes require a restart and are logged.
func (m *worldManager) Reload() error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	var config *knowledgeconfig.Config
	var err error
	if m.dynamicSet {
		config, err = knowledgeconfig.LoadWithFragment(m.configFile, m.dynamic)
	} else {
		config, err = knowledgeconfig.Load(m.configFile)
	}
	if err != nil {
		return err
	}
	return m.apply(config.Worlds)
}

// SetDynamicWorlds applies a worlds fragment ahead of the mounted worldsFile:
// the in-process provisioner's registry projection. It stays in force across
// file reloads; the file remains what a restart reads, so one is required.
func (m *worldManager) SetDynamicWorlds(fragment []byte) error {
	if !m.resilient {
		return errors.New("no worldsFile configured; dynamic worlds would not survive a restart")
	}
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	config, err := knowledgeconfig.LoadWithFragment(m.configFile, fragment)
	if err != nil {
		return err
	}
	if err := m.apply(config.Worlds); err != nil {
		return err
	}
	m.dynamic, m.dynamicSet = slices.Clone(fragment), true
	return nil
}

// retiredWorld is a runtime out of m.entries that the router may still reach.
type retiredWorld struct {
	name  string
	entry *worldEntry
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
		delete(m.entries, name)
		if !entry.published {
			m.retireEntryLocked(name, entry)
			continue
		}
		// Closed by the next publish that succeeds, so the router never
		// points at a closed runtime.
		m.retiring = append(m.retiring, retiredWorld{name: name, entry: entry})
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

// openLocked opens one world: store, policy seed, runtime, and its
// token-file watcher.
func (m *worldManager) openLocked(world *knowledgeconfig.WorldConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), worldAddTimeout)
	defer cancel()

	// Before the store: an unusable policy file must fail the open without
	// leaving a half-made world behind.
	seed, err := policySeed(world)
	if err != nil {
		return fmt.Errorf("policy seed: %w", err)
	}
	store, err := m.openStore(ctx, world, storeHooks{
		logger:    m.logger.With("world", world.Name),
		committed: m.committed(world.ID()),
	})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	runtime, err := m.serve(ctx, world, store, seed)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			m.logger.Warn("world store close failed", "world", world.Name, "error", closeErr)
		}
		return err
	}
	for _, authority := range world.Authorities {
		if err := m.certs.Covers(authority); err != nil {
			m.logger.Warn("TLS certificate does not cover world authority; verifying clients will fail until the cert rotates",
				"world", world.Name, "authority", authority, "err", err)
		}
	}
	m.entries[world.Name] = &worldEntry{config: *world, runtime: runtime, store: store}
	m.acquireTokenWatchLocked(tokenFiles(world))
	m.logger.Info("world opened", "world", world.Name)
	return nil
}

// serve seeds the world's policy and builds its runtime, which owns store
// from then on; on an error the caller still owns it.
func (m *worldManager) serve(
	ctx context.Context,
	world *knowledgeconfig.WorldConfig,
	store worldStore,
	seed writepolicy.PolicySeed,
) (*worldruntime.Runtime, error) {
	// Every world holds a usable policy before it serves a write. A provisioned
	// world's broker replaces the marked seed with its own as version two.
	created, err := writepolicy.Ensure(ctx, store, seed)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if created {
		m.logger.Info("seeded the initial write policy", "world", world.Name, "path", publishpolicy.DocumentPath)
	}
	runtime, err := worldruntime.New(&worldruntime.Config{
		Name:             world.Name,
		Store:            writepolicy.Enforce(store, writepolicy.Options{Require: true}),
		CloseBackend:     store.Close,
		Changes:          store.Changes(),
		MaxWatches:       world.Limits.MaxWatches,
		MaxStreams:       m.maxStreams,
		TokensFile:       world.Auth.TokensFile,
		StaticTokensFile: world.Auth.StaticTokensFile,
		// Knowledge worlds are public-read, so a tokens Secret that lands
		// after the open only delays writes; the watcher reloads it.
		OptionalTokensFiles: true,
		DisableTokenWatch:   true, // the coordinator owns reloads
		ReadOnly:            world.ReadOnly,
		RequestTimeout:      time.Duration(world.Limits.RequestTimeout),
		MaxConcurrent:       world.Limits.MaxConcurrentRequests,
		RateLimit:           world.Limits.RequestsPerSecond,
		RateBurst:           world.Limits.Burst,
		Logger:              m.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: %w", err)
	}
	return runtime, nil
}

// committed is the store hook that hints peers about this replica's commits.
func (m *worldManager) committed(worldID string) func(sequence int64) {
	if m.hint == nil {
		return nil
	}
	return func(sequence int64) { m.hint(worldID, sequence) }
}

// Hint is a peer replica saying it committed a world through a sequence; a
// store several replicas share decides whether to read.
func (m *worldManager) Hint(hint peerhint.Hint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.entries {
		if entry.config.ID() != hint.WorldID {
			continue
		}
		if follower, ok := entry.store.(backend.Follower); ok {
			follower.Follow(hint.Sequence)
		}
	}
}

// policySeed picks the world's initial policy: the operator's file when
// configured, else the embedded default.
func policySeed(world *knowledgeconfig.WorldConfig) (writepolicy.PolicySeed, error) {
	if world.Policy.File == "" {
		return knowledgeseed.DefaultPolicySeed(), nil
	}
	return knowledgeseed.PolicySeedFromFile(world.Policy.File)
}

// dirWatch is one refcounted configwatch watcher covering every world
// whose tokens file lives in the same directory.
type dirWatch struct {
	cancel context.CancelFunc
	// files refcounts every token file in the directory across worlds; the
	// watcher consults it per event, so a world added later is covered.
	mu    sync.Mutex
	files map[string]int
}

func (watch *dirWatch) add(files []string) {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	for _, file := range files {
		watch.files[filepath.Clean(file)]++
	}
}

// remove drops files and reports whether the directory has none left.
func (watch *dirWatch) remove(files []string) bool {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	for _, file := range files {
		clean := filepath.Clean(file)
		watch.files[clean]--
		if watch.files[clean] <= 0 {
			delete(watch.files, clean)
		}
	}
	return len(watch.files) == 0
}

func (watch *dirWatch) has(cleanPath string) bool {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	return watch.files[cleanPath] > 0
}

// tokenFiles lists a world's configured token files. The chart projects
// both into one directory, which the watcher below relies on.
func tokenFiles(world *knowledgeconfig.WorldConfig) []string {
	return auth.SourceConfig{TokensFile: world.Auth.TokensFile, StaticTokensFile: world.Auth.StaticTokensFile}.Files()
}

// acquireTokenWatchLocked starts (or shares) the watcher for the token
// files' parent directory. The reload is the global coordinator pass, so
// one watcher per directory is exactly as fresh as one per world.
func (m *worldManager) acquireTokenWatchLocked(files []string) {
	if len(files) == 0 {
		return
	}
	dir := filepath.Dir(files[0])
	if watch, ok := m.tokenDirs[dir]; ok {
		watch.add(files)
		return
	}
	watchCtx, cancel := context.WithCancel(m.watchCtx)
	watch := &dirWatch{cancel: cancel, files: make(map[string]int, len(files))}
	watch.add(files)
	m.tokenDirs[dir] = watch
	watcher := &configwatch.Watcher{Targets: files[:1], Relevant: watch.has, Reload: m.tokens.Reload, Logger: m.logger}
	m.group.Go(func() {
		if err := watcher.Run(watchCtx); err != nil {
			m.logger.Warn("token watcher exited", "dir", dir, "error", err)
		}
	})
}

func (m *worldManager) releaseTokenWatchLocked(files []string) {
	if len(files) == 0 {
		return
	}
	dir := filepath.Dir(files[0])
	watch, ok := m.tokenDirs[dir]
	if !ok {
		return
	}
	if !watch.remove(files) {
		return
	}
	// No join: waiting under m.mu would stall every reload behind a
	// slow watcher exit; the shared group collects the goroutine and a
	// briefly overlapping successor only adds an idempotent reload.
	watch.cancel()
	delete(m.tokenDirs, dir)
}

func (m *worldManager) closeEntryLocked(name string, entry *worldEntry) {
	delete(m.entries, name)
	m.retireEntryLocked(name, entry)
}

// retireEntryLocked closes an entry already removed from m.entries.
func (m *worldManager) retireEntryLocked(name string, entry *worldEntry) {
	if m.beforeRetire != nil {
		m.beforeRetire(name)
	}
	m.releaseTokenWatchLocked(tokenFiles(&entry.config))
	if err := entry.runtime.Close(); err != nil {
		m.logger.Warn("world runtime close failed", "world", name, "error", err)
	}
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
		// A world with no token files has no store to coordinate: reads are
		// public and writes come through the identity grant.
		if files := &entry.config.Auth; files.TokensFile != "" || files.StaticTokensFile != "" {
			worlds = append(worlds, tokenWorld{name: name, runtime: entry.runtime})
		}
	}
	// Prevalidate tokens, then let SwapWith pair routing with the token
	// commit (it rolls routing back if the commit loses the tiny
	// validate-to-commit race).
	if err := m.tokens.ValidateWorlds(worlds); err != nil {
		return err
	}
	if err := m.router.SwapWith(mappings, func() error {
		return m.tokens.SetWorlds(worlds)
	}); err != nil {
		return fmt.Errorf("publish world views: %w", err)
	}
	for _, entry := range m.entries {
		entry.published = true
	}
	// The router left them only now; a failed publish keeps them serving.
	for _, retired := range m.retiring {
		m.retireEntryLocked(retired.name, retired.entry)
	}
	m.retiring = nil
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
		if err := m.reloadStagedTokensLocked(); err != nil {
			m.logger.Error("staged token reload failed; publish deferred", "error", err)
			m.needsPublish = true
			return
		}
		if err := m.publishLocked(); err != nil {
			m.logger.Error("publish after world retry failed", "error", err)
			m.needsPublish = true
			return
		}
		m.needsPublish = false
	}
}

// reloadStagedTokensLocked refreshes runtimes no publish has carried yet; the
// coordinator reloads published worlds only. A failure blocks the publish:
// going live with the old snapshot could serve replaced credentials.
func (m *worldManager) reloadStagedTokensLocked() error {
	var errs []error
	for name, entry := range m.entries {
		if entry.published {
			continue
		}
		if err := entry.runtime.ReloadTokens(); err != nil {
			errs = append(errs, fmt.Errorf("world %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
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

// Drain tells every world's open watches closing ahead of a listener drain.
func (m *worldManager) Drain() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.entries {
		entry.runtime.Drain()
	}
	for _, retired := range m.retiring {
		retired.entry.runtime.Drain()
	}
}

// Close closes every live world through the one teardown path.
func (m *worldManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, entry := range m.entries {
		m.closeEntryLocked(name, entry)
	}
	for _, retired := range m.retiring {
		m.retireEntryLocked(retired.name, retired.entry)
	}
	m.retiring = nil
}
