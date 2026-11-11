package knowledgeserver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
	"github.com/latebit-io/demarkus/server/internal/worldstore"
)

// retiredWorld is a runtime out of m.entries that the router may still reach.
type retiredWorld struct {
	name  string
	entry *worldEntry
}

// errManagerClosed refuses a world set applied after Close.
var errManagerClosed = errors.New("world manager is closed")

// apply diffs desired against live. In resilient mode per-world failures
// are logged and retried; in static mode they are returned. A world that
// fails to open never takes down the one it would have replaced.
func (m *worldManager) apply(desired []knowledgeconfig.WorldConfig) error {
	m.appliers.Add(1)
	m.yieldRetry()
	m.applyMu.Lock()
	m.appliers.Add(-1)
	defer m.applyMu.Unlock()
	if m.closed {
		return errManagerClosed
	}

	opens, errs := m.plan(desired)
	for index := range opens {
		target := &opens[index]
		err := m.bring(m.watchCtx, target)
		switch {
		case err == nil:
		case !m.resilient:
			errs = append(errs, fmt.Errorf("open world %q: %w", target.Name, err))
		default:
			m.logger.Warn("world open failed; will retry", "world", target.Name, "err", err)
			m.setPending(target)
		}
	}
	// Publish unconditionally: the plan already dropped removed worlds, so
	// returning early would keep routing to them.
	return errors.Join(append(errs, m.publish())...)
}

// plan drops the worlds desired no longer holds and returns the new or
// changed ones to open. A world pending under the same config stays with the
// retry loop, so an unreachable store delays no other apply. errs are refusals.
func (m *worldManager) plan(desired []knowledgeconfig.WorldConfig) (opens []knowledgeconfig.WorldConfig, errs []error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	want := make(map[string]*knowledgeconfig.WorldConfig, len(desired))
	for index := range desired {
		target := &desired[index]
		if m.ungatedLocked(target) {
			// What is live under the name, if anything, keeps serving gated.
			err := fmt.Errorf("world %q %w", target.Name, errUngated)
			m.logger.Error("world refused", "world", target.Name, "err", err)
			if !m.resilient {
				errs = append(errs, err)
			}
			if live, ok := m.entries[target.Name]; ok {
				want[target.Name] = &live.config
			}
			continue
		}
		want[target.Name] = target
	}

	for name, entry := range m.entries {
		if _, keep := want[name]; !keep {
			m.logger.Info("world removed", "world", name)
			m.dropEntryLocked(name, entry)
		}
	}
	for name := range m.pending {
		if _, keep := want[name]; !keep {
			delete(m.pending, name)
		}
	}
	for name, target := range want {
		if live, ok := m.entries[name]; ok && reflect.DeepEqual(live.config, *target) {
			// Back to what is live: nothing left to retry.
			delete(m.pending, name)
			continue
		}
		if waiting, ok := m.pending[name]; ok && reflect.DeepEqual(waiting, *target) {
			continue
		}
		if _, live := m.entries[name]; live {
			m.logger.Info("world config changed; replacing runtime", "world", name)
		}
		opens = append(opens, *target)
	}
	return opens, errs
}

// bring opens target and puts it in force. What is live under its name keeps
// serving until the replacement is open, and for good if it fails to.
func (m *worldManager) bring(ctx context.Context, target *knowledgeconfig.WorldConfig) error {
	m.mu.Lock()
	previous := m.entries[target.Name]
	m.mu.Unlock()
	if previous != nil && previous.config.IsFile() {
		return m.replaceFileWorld(ctx, target, previous)
	}
	entry, err := m.open(ctx, target)
	if err != nil {
		return err
	}
	m.install(entry)
	return nil
}

// replaceFileWorld closes previous before target opens: a file store keeps
// its index and journal in memory, so two on one directory miss each other's
// writes. The world leaves the router first; a failed open restores previous.
func (m *worldManager) replaceFileWorld(ctx context.Context, target *knowledgeconfig.WorldConfig, previous *worldEntry) error {
	name := target.Name
	m.mu.Lock()
	routed := previous.published
	m.dropEntryLocked(name, previous)
	if routed {
		if err := m.publishLocked(); err != nil {
			// Still routed, so it cannot close: put it back as it was.
			m.reinstateLocked(name, previous)
			m.mu.Unlock()
			return fmt.Errorf("unroute before the replacement: %w", err)
		}
	}
	m.mu.Unlock()

	entry, err := m.open(ctx, target)
	if err == nil {
		m.install(entry)
		return nil
	}
	restored, restoreErr := m.open(ctx, &previous.config)
	if restoreErr != nil {
		return errors.Join(err, fmt.Errorf("reopen the previous configuration: %w", restoreErr))
	}
	m.install(restored)
	return err
}

// reinstateLocked undoes dropEntryLocked for an entry that never closed.
func (m *worldManager) reinstateLocked(name string, entry *worldEntry) {
	for index, retired := range m.retiring {
		if retired.entry == entry {
			m.retiring = append(m.retiring[:index], m.retiring[index+1:]...)
			break
		}
	}
	m.entries[name] = entry
	m.setPolicyStore(name, entry.store)
}

// open builds one world outside m.mu: store, policy seed and runtime. ctx
// ends it at shutdown; nothing routes to the entry until it is installed.
func (m *worldManager) open(ctx context.Context, world *knowledgeconfig.WorldConfig) (*worldEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, worldAddTimeout)
	defer cancel()

	// Before the store: an unusable policy file must fail the open without
	// leaving a half-made world behind.
	seed, err := policySeed(world)
	if err != nil {
		return nil, fmt.Errorf("policy seed: %w", err)
	}
	store, err := m.openStore(ctx, world, worldstore.Hooks{
		Logger:     m.logger.With("world", world.Name),
		Committed:  m.committed(world.ID()),
		Compaction: m.compaction,
	})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	runtime, err := m.serve(ctx, world, store, seed)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			m.logger.Warn("world store close failed", "world", world.Name, "error", closeErr)
		}
		return nil, err
	}
	for _, authority := range world.PinnedAuthorities() {
		if err := m.certs.Covers(authority); err != nil {
			m.logger.Warn("TLS certificate does not cover world authority; verifying clients will fail until the cert rotates",
				"world", world.Name, "authority", authority, "err", err)
		}
	}
	return &worldEntry{config: *world, runtime: runtime, store: store}, nil
}

// install puts an opened entry in force under its name; what it replaces
// closes at the next publish, or at once if nothing routes to it.
func (m *worldManager) install(entry *worldEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := entry.config.Name
	if previous, live := m.entries[name]; live {
		m.dropEntryLocked(name, previous)
	}
	m.entries[name] = entry
	m.setPolicyStore(name, entry.store)
	m.acquireTokenWatchLocked(tokenFiles(&entry.config))
	delete(m.pending, name)
	m.logger.Info("world opened", "world", name)
}

// dropEntryLocked takes an entry out of the live set. A published one waits
// for the next publish that succeeds, so the router never points at a
// closed runtime; one nothing routes to closes at once.
func (m *worldManager) dropEntryLocked(name string, entry *worldEntry) {
	delete(m.entries, name)
	m.setPolicyStore(name, nil)
	if !entry.published {
		m.retireEntryLocked(name, entry)
		return
	}
	m.retiring = append(m.retiring, retiredWorld{name: name, entry: entry})
}

func (m *worldManager) setPending(world *knowledgeconfig.WorldConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[world.Name] = *world
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

// publish republishes the views. Dynamic mode degrades like a failed open:
// log, mark for the retry loop, keep the process alive.
func (m *worldManager) publish() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.publishLocked()
	if err != nil && m.resilient {
		m.logger.Error("world publish failed; will retry", "error", err)
		m.needsPublish = true
		return nil
	}
	return err
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

// yieldRetry ends the retry loop's opens in flight so an apply need not
// wait them out; what they were opening stays pending.
func (m *worldManager) yieldRetry() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.yield != nil {
		m.yield()
	}
}

// retryContext is the retry loop's open context: shutdown ends it, and so
// does an apply through yieldRetry. done releases it.
func (m *worldManager) retryContext() (ctx context.Context, done func()) {
	ctx, cancel := context.WithCancel(m.watchCtx)
	m.mu.Lock()
	m.yield = cancel
	m.mu.Unlock()
	return ctx, func() {
		m.mu.Lock()
		m.yield = nil
		m.mu.Unlock()
		cancel()
	}
}

// retryPending opens the pending worlds and republishes. It gives way to an
// apply, which takes the world set from there.
func (m *worldManager) retryPending() {
	if !m.applyMu.TryLock() {
		return
	}
	defer m.applyMu.Unlock()
	ctx, done := m.retryContext()
	defer done()
	// After the context is in place: an apply arriving from here on cancels it.
	if m.closed || m.appliers.Load() > 0 {
		return
	}

	m.mu.Lock()
	waiting := make([]knowledgeconfig.WorldConfig, 0, len(m.pending))
	for name := range m.pending {
		waiting = append(waiting, m.pending[name])
	}
	recovered := m.needsPublish
	m.mu.Unlock()

	for index := range waiting {
		if ctx.Err() != nil || m.appliers.Load() > 0 {
			break
		}
		world := &waiting[index]
		if err := m.bring(ctx, world); err != nil {
			// A yielded open is not a failure worth a line.
			if ctx.Err() == nil {
				m.logger.Warn("world open retry failed", "world", world.Name, "err", err)
			}
			continue
		}
		recovered = true
	}
	if recovered {
		m.republish()
	}
}

// republish is the retry loop's publish, staged tokens first.
func (m *worldManager) republish() {
	m.mu.Lock()
	defer m.mu.Unlock()
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
