package knowledgeserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/stylerules"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/certsource"
	"github.com/latebit-io/demarkus/server/internal/knowledge/knowledgeseed"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/peerhint"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
	"github.com/latebit-io/demarkus/server/internal/worldstore"
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
	openStore  worldstore.Factory
	// hint tells peer replicas a world committed a sequence; a no-op
	// without peers.
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

	// applyMu serializes changes to the world set: a config apply and the
	// retry loop. Worlds open under it and outside mu, so a slow store never
	// holds up hints, a drain or a count.
	applyMu sync.Mutex
	// closed refuses a world set applied after Close; under applyMu.
	closed bool
	// appliers counts applies waiting for applyMu: the retry loop yields.
	appliers atomic.Int32

	mu      sync.Mutex
	entries map[string]*worldEntry
	// yield cancels the retry loop's opens when an apply arrives.
	yield context.CancelFunc
	// mustGate names the authorities the broker serves as private worlds,
	// which must be gated; nil without a broker.
	mustGate func(authority string) bool
	// fragmentWarning is the last generated-fragment warning logged.
	fragmentWarning string
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
	// classify stamps written bodies with the classifier's verdict; nil
	// when no classifier is configured. One stamper serves every world.
	classify func(ctx context.Context, path, body string) string
	// reloadMu orders load-then-apply, so a file reload racing a push can
	// never apply a pre-push world set after it; the push state lives under it.
	reloadMu sync.Mutex
	// dynamic is a private copy of the last accepted push, in force once
	// dynamicSet; an empty push is a push, not an absence.
	dynamic    []byte
	dynamicSet bool

	// floorWorld's policy is merged under every other world's; "" is none.
	floorWorld string
	// compaction is this replica's part in checkpointing shared stores.
	compaction knowledgeconfig.CompactionRole
	content    writepolicy.ContentOptions
	// policyStores mirrors the live stores for the write path, which reads
	// a sibling world's policy inside a commit. Its own lock, never held
	// across a close: runtime.Close drains in-flight writes under m.mu.
	policyMu     sync.RWMutex
	policyStores map[string]worldstore.Store
}

type worldEntry struct {
	config  knowledgeconfig.WorldConfig
	runtime *worldruntime.Runtime
	// store is owned by runtime, which closes it.
	store worldstore.Store
	// published is set once a router publish carried this runtime; until
	// then nothing routes to it and it can close at once.
	published bool
}

// worldManagerConfig is what a world manager is built from.
type worldManagerConfig struct {
	configFile string
	config     *knowledgeconfig.Config
	openStore  worldstore.Factory
	certs      *certsource.Source
	// peers carries commit hints to and from the other replicas.
	peers  *peerLinks
	logger *slog.Logger
	judges JudgeFactory
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
		floorWorld: config.FloorWorld,
		compaction: config.Compaction.Role,
		content: writepolicy.ContentOptions{Options: contentcheck.Options{
			PII:         config.ContentCheck.PII,
			WorkDomains: config.ContentCheck.WorkDomains,
			Steering:    config.ContentCheck.Injection,
		}, NearDuplicate: config.ContentCheck.NearDuplicate},
		policyStores: make(map[string]worldstore.Store),
	}
	classify, err := newClassify(config.ContentCheck.Classifier, cfg.judges, m.logger)
	if err != nil {
		return nil, err
	}
	m.classify = classify
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
	m.noteFragment(config.FragmentWarning)
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
	m.noteFragment(config.FragmentWarning)
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
	m.noteFragment(config.FragmentWarning)
	if err := m.apply(config.Worlds); err != nil {
		return err
	}
	m.dynamic, m.dynamicSet = slices.Clone(fragment), true
	return nil
}

// serve seeds the world's policy and builds its runtime, which owns store
// from then on; on an error the caller still owns it.
func (m *worldManager) serve(
	ctx context.Context,
	world *knowledgeconfig.WorldConfig,
	store worldstore.Store,
	seed writepolicy.PolicySeed,
) (*worldruntime.Runtime, error) {
	// Every world with a required policy holds a usable one before it serves
	// a write, seeded here and nowhere else.
	if !world.Policy.Optional {
		created, err := writepolicy.Ensure(ctx, store, seed)
		if err != nil {
			return nil, fmt.Errorf("policy: %w", err)
		}
		if created {
			m.logger.Info("seeded the initial write policy", "world", world.Name, "path", publishpolicy.DocumentPath, "seed", world.Seed)
		}
	}
	if world.Seed == knowledgeconfig.SeedMemory {
		// A failed upgrade leaves the world on the usable policy it has.
		upgraded, err := knowledgeseed.UpgradeToMemory(ctx, store)
		if err != nil {
			m.logger.Warn("memory policy upgrade failed; the world keeps its policy", "world", world.Name, "err", err)
		}
		if upgraded {
			m.logger.Info("replaced the default policy seed with the memory policy", "world", world.Name)
		}
	}
	if world.Name == m.floorWorld && !world.ReadOnly {
		// The floor world also carries the style guide every finding cites.
		created, err := knowledgeseed.EnsureStyleGuide(ctx, store)
		if err != nil {
			return nil, fmt.Errorf("style guide: %w", err)
		}
		if created {
			m.logger.Info("seeded the style guide", "world", world.Name, "path", stylerules.DocumentPath)
		}
	}
	runtime, err := worldruntime.New(&worldruntime.Config{
		Name:             world.Name,
		Store:            writepolicy.Enforce(store, m.enforceOptions(world)),
		CloseBackend:     store.Close,
		Changes:          store.Changes(),
		MaxWatches:       world.Limits.MaxWatches,
		MaxStreams:       m.maxStreams,
		TokensFile:       world.Auth.TokensFile,
		StaticTokensFile: world.Auth.StaticTokensFile,
		// Knowledge worlds are public-read, so a tokens Secret that lands
		// after the open only delays writes; the watcher reloads it. A soul
		// requires its file.
		OptionalTokensFiles: !world.Auth.Required,
		DisableTokenWatch:   true, // the coordinator owns reloads
		ReadOnly:            world.ReadOnly,
		RestrictGovernance:  world.Policy.Governance != knowledgeconfig.GovernanceOpen,
		RequireGate:         world.Gated,
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

// committed is the store hook that hints peers about the store's commits.
func (m *worldManager) committed(worldID string) func(sequence int64) {
	return func(sequence int64) { m.hint(worldID, sequence) }
}

// Hint is a peer replica saying a world committed through a sequence; a
// store several replicas share decides whether to read.
func (m *worldManager) Hint(hint peerhint.Hint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.entries {
		// A file world has no ID; a hint can never be for it.
		if entry.config.ID() == "" || entry.config.ID() != hint.WorldID {
			continue
		}
		if follower, ok := entry.store.(backend.Follower); ok {
			follower.Follow(hint.Sequence)
		}
	}
}

// policySeed picks the world's initial policy: the operator's file when
// configured, else the embedded default; an optional policy has no seed.
func policySeed(world *knowledgeconfig.WorldConfig) (writepolicy.PolicySeed, error) {
	switch {
	case world.Policy.Optional:
		return writepolicy.PolicySeed{}, nil
	case world.Seed == knowledgeconfig.SeedMemory:
		return knowledgeseed.MemoryPolicySeed(), nil
	case world.Policy.File == "":
		return knowledgeseed.DefaultPolicySeed(), nil
	}
	return knowledgeseed.PolicySeedFromFile(world.Policy.File)
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

// Close closes every live world through the one teardown path and refuses
// any world set applied afterwards. The caller ends watchCtx first, which
// ends an open still in flight.
func (m *worldManager) Close() {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.closed = true
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, entry := range m.entries {
		delete(m.entries, name)
		m.setPolicyStore(name, nil)
		m.retireEntryLocked(name, entry)
	}
	for _, retired := range m.retiring {
		m.retireEntryLocked(retired.name, retired.entry)
	}
	m.retiring = nil
}

// noteFragment logs a generated fragment's skipped keys once per change, so
// a rollout that mixes builds says so without repeating it on every reload.
func (m *worldManager) noteFragment(warning string) {
	m.mu.Lock()
	changed := warning != m.fragmentWarning
	m.fragmentWarning = warning
	m.mu.Unlock()
	if changed && warning != "" {
		m.logger.Warn("worlds fragment carries keys this build does not know; they are skipped", "detail", warning)
	}
}

// requireGate installs the broker's rule and refuses it when a live world
// already breaks it: a private world must never have been served anonymously.
func (m *worldManager) requireGate(must func(authority string) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mustGate = must
	var errs []error
	for name, entry := range m.entries {
		if m.ungatedLocked(&entry.config) {
			errs = append(errs, fmt.Errorf("world %q %w", name, errUngated))
		}
	}
	return errors.Join(errs...)
}

// errUngated is a world the broker serves as private that the capability
// listener would answer anonymously.
var errUngated = errors.New("must be gated: the broker serves it as a private world; set gated: true on it")

// ungatedLocked reports whether world breaks the broker's rule.
func (m *worldManager) ungatedLocked(world *knowledgeconfig.WorldConfig) bool {
	return m.mustGate != nil && !world.Gated && slices.ContainsFunc(world.Authorities, m.mustGate)
}
