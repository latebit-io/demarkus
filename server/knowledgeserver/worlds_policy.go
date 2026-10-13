package knowledgeserver

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// enforceOptions judges world's writes: the content scan everywhere, the
// floor world's policy under every other world's, and on the floor world a
// check that a new floor leaves every world satisfiable.
func (m *worldManager) enforceOptions(world string) writepolicy.Options {
	options := writepolicy.Options{Require: true, Content: m.content}
	switch {
	case m.floorWorld == "":
	case world == m.floorWorld:
		options.Worlds = m.worldPolicies
	default:
		options.Floor = m.floorPolicy
	}
	return options
}

// setPolicyStore publishes (or with nil retracts) a live world's store to
// the write path.
func (m *worldManager) setPolicyStore(name string, store worldStore) {
	m.policyMu.Lock()
	defer m.policyMu.Unlock()
	if store == nil {
		delete(m.policyStores, name)
		return
	}
	m.policyStores[name] = store
}

// floorPolicy reads the floor world's current policy. A floor world that is
// configured but not open is an error, so the write is refused rather than
// judged without its floor.
func (m *worldManager) floorPolicy(ctx context.Context) (publishpolicy.Policy, error) {
	m.policyMu.RLock()
	store, open := m.policyStores[m.floorWorld]
	m.policyMu.RUnlock()
	if !open {
		return publishpolicy.Policy{}, fmt.Errorf("floor world %q is not open", m.floorWorld)
	}
	return writepolicy.Read(ctx, store, false)
}

// worldPolicies reads every other live world's policy, for judging a
// candidate floor. A world without a policy document contributes the empty
// policy.
func (m *worldManager) worldPolicies(ctx context.Context) ([]publishpolicy.Policy, error) {
	m.policyMu.RLock()
	stores := make(map[string]worldStore, len(m.policyStores))
	for name, store := range m.policyStores {
		if name != m.floorWorld {
			stores[name] = store
		}
	}
	m.policyMu.RUnlock()
	policies := make([]publishpolicy.Policy, 0, len(stores))
	for name, store := range stores {
		policy, err := writepolicy.Read(ctx, store, false)
		if err != nil {
			return nil, fmt.Errorf("world %q: %w", name, err)
		}
		policies = append(policies, policy)
	}
	return policies, nil
}
