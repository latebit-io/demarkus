package main

import (
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
	stores := make([]*auth.TokenStore, len(worlds))
	for index := range worlds {
		stores[index] = worlds[index].runtime.Tokens()
	}
	if err := validateUniqueTokens(coordinator.worlds, stores); err != nil {
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
	stores := make([]*auth.TokenStore, len(replacement))
	for index := range replacement {
		stores[index] = replacement[index].runtime.Tokens()
	}
	if err := validateUniqueTokens(replacement, stores); err != nil {
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

func (coordinator *tokenCoordinator) Reload() error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()

	candidates := make([]*auth.TokenStore, len(coordinator.worlds))
	for index, world := range coordinator.worlds {
		store, err := world.runtime.LoadTokens()
		if err != nil {
			return fmt.Errorf("load tokens for world %q: %w", world.name, err)
		}
		if store == nil {
			return fmt.Errorf("load tokens for world %q: token store is nil", world.name)
		}
		candidates[index] = store
	}
	if err := validateUniqueTokens(coordinator.worlds, candidates); err != nil {
		return err
	}
	if err := coordinator.validateTransition(candidates); err != nil {
		return err
	}
	for index, world := range coordinator.worlds {
		if err := world.runtime.PublishTokens(candidates[index]); err != nil {
			return fmt.Errorf("publish tokens for world %q: %w", world.name, err)
		}
	}
	return nil
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
