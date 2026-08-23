package main

import (
	"errors"
	"fmt"
	"sync"

	"github.com/latebit-io/demarkus/server/internal/auth"
)

type tokenRuntime interface {
	Tokens() *auth.TokenStore
	LoadTokens() (*auth.TokenStore, error)
	PublishTokens(*auth.TokenStore)
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
	if err := coordinator.validateUnique(stores); err != nil {
		return nil, err
	}
	return coordinator, nil
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
	if err := coordinator.validateUnique(candidates); err != nil {
		return err
	}
	if err := coordinator.validateTransition(candidates); err != nil {
		return err
	}
	for index, world := range coordinator.worlds {
		world.runtime.PublishTokens(candidates[index])
	}
	return nil
}

func (coordinator *tokenCoordinator) validateUnique(stores []*auth.TokenStore) error {
	owners := make(map[string]int)
	for worldIndex, store := range stores {
		if store == nil {
			return fmt.Errorf("world %q has no token store", coordinator.worlds[worldIndex].name)
		}
		for _, hash := range store.Hashes() {
			if owner, exists := owners[hash]; exists {
				return fmt.Errorf("token hash is shared by worlds %q and %q", coordinator.worlds[owner].name, coordinator.worlds[worldIndex].name)
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
				return errors.New("live token transfer between worlds is unsafe; restart with the new token assignment")
			}
		}
	}
	return nil
}
