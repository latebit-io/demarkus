package knowledgeserver

import (
	"errors"
	"fmt"
	"sync"

	"github.com/latebit-io/demarkus/server/internal/auth"
)

type tokenRuntime interface {
	Tokens() *auth.TokenStore
	LoadTokens() (*auth.TokenStore, error)
	PublishTokens(*auth.TokenStore) error
}

type tokenWorld struct {
	name    string
	runtime tokenRuntime
}

type tokenCoordinator struct {
	mu     sync.Mutex
	worlds []tokenWorld
}

func newTokenCoordinator(worlds []tokenWorld) (*tokenCoordinator, error) {
	coordinator := &tokenCoordinator{worlds: append([]tokenWorld(nil), worlds...)}
	if err := coordinator.ValidateWorlds(coordinator.worlds); err != nil {
		return nil, err
	}
	return coordinator, nil
}

// SetWorlds replaces the coordinated world set (hot-reload seam). The
// new set's current stores must satisfy the cross-world uniqueness
// invariant before the swap publishes.
func (coordinator *tokenCoordinator) SetWorlds(worlds []tokenWorld) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	replacement := append([]tokenWorld(nil), worlds...)
	if err := coordinator.ValidateWorlds(replacement); err != nil {
		return err
	}
	coordinator.worlds = replacement
	return nil
}

// ValidateWorlds checks a candidate world set against the cross-world
// token-uniqueness invariant without publishing it (prevalidation seam).
func (coordinator *tokenCoordinator) ValidateWorlds(worlds []tokenWorld) error {
	stores := make([]*auth.TokenStore, len(worlds))
	for index := range worlds {
		stores[index] = worlds[index].runtime.Tokens()
	}
	return validateUniqueTokens(worlds, stores)
}

// Reload publishes the worlds whose token files load and reports the rest,
// which keep their tokens: one broken file never holds back another world's
// revocation. A cross-world check failure publishes nothing.
func (coordinator *tokenCoordinator) Reload() error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()

	candidates := make([]*auth.TokenStore, len(coordinator.worlds))
	loaded := make([]bool, len(coordinator.worlds))
	var failed []error
	for index, world := range coordinator.worlds {
		store, err := world.runtime.LoadTokens()
		if err == nil && store == nil {
			err = errors.New("token store is nil")
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("load tokens for world %q: %w", world.name, err))
			candidates[index] = world.runtime.Tokens()
			continue
		}
		candidates[index], loaded[index] = store, true
	}
	if err := validateUniqueTokens(coordinator.worlds, candidates); err != nil {
		return errors.Join(append(failed, err)...)
	}
	if err := coordinator.validateTransition(candidates); err != nil {
		return errors.Join(append(failed, err)...)
	}
	for index, world := range coordinator.worlds {
		if !loaded[index] {
			continue
		}
		if err := world.runtime.PublishTokens(candidates[index]); err != nil {
			failed = append(failed, fmt.Errorf("publish tokens for world %q: %w", world.name, err))
		}
	}
	return errors.Join(failed...)
}

func validateUniqueTokens(worlds []tokenWorld, stores []*auth.TokenStore) error {
	owners := make(map[string]int)
	for worldIndex, store := range stores {
		if store == nil {
			return fmt.Errorf("world %q has no token store", worlds[worldIndex].name)
		}
		for _, hash := range store.Hashes() {
			if owner, exists := owners[hash]; exists {
				return fmt.Errorf("token hash is shared by worlds %q and %q", worlds[owner].name, worlds[worldIndex].name)
			}
			owners[hash] = worldIndex
		}
	}
	return nil
}

func (coordinator *tokenCoordinator) validateTransition(candidates []*auth.TokenStore) error {
	currentOwners := make(map[string]int)
	for worldIndex, world := range coordinator.worlds {
		for _, hash := range world.runtime.Tokens().Hashes() {
			currentOwners[hash] = worldIndex
		}
	}
	for worldIndex, store := range candidates {
		for _, hash := range store.Hashes() {
			owner, exists := currentOwners[hash]
			if exists && owner != worldIndex {
				return fmt.Errorf(
					"live token transfer from world %q to world %q is unsafe; restart with the new token assignment",
					coordinator.worlds[owner].name,
					coordinator.worlds[worldIndex].name,
				)
			}
		}
	}
	return nil
}
