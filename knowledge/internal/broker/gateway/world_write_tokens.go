package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
)

// worldWriteTokenLabel names the broker's entry in a world's tokens.toml.
// Stable, so re-provisioning hits the same entry instead of piling up dead ones.
func worldWriteTokenLabel(worldName string) string {
	return "broker-write-" + worldName
}

// WorldWriteTokenStore provisions and caches one long lived write token per
// world: the hash goes into the world's tokens.toml, the raw token into a
// per world broker Secret so every pod converges on the same value.
type WorldWriteTokenStore struct {
	cfg   *core.Config
	store core.SecretStore
	clock func() time.Time

	mu    sync.Mutex
	cache map[string]string // worldName → raw token
}

func newWorldWriteTokenStore(cfg *core.Config, store core.SecretStore) *WorldWriteTokenStore {
	s := &WorldWriteTokenStore{
		cfg:   cfg,
		store: store,
		clock: time.Now,
		cache: make(map[string]string),
	}
	// A dropped world's token record is deleted with it; the cached copy is stale.
	cfg.Registry().OnDrop(s.Invalidate)
	return s
}

// Get returns the cached raw write token for worldName, or ok=false
// if this broker pod has not provisioned the world yet. Callers
// that need the token MUST fall through to Provision on miss —
// Get is a fast-path read for hot dispatch loops.
func (s *WorldWriteTokenStore) Get(worldName string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.cache[worldName]
	return tok, ok
}

// Invalidate drops the cached token so the next Provision re-reads the
// broker Secret and reconciles the world's tokens.toml (storage.SyncWorldHash).
// Called on 401: the world's tokens Secret may have been reset or rotated.
func (s *WorldWriteTokenStore) Invalidate(worldName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, worldName)
}

// Provision returns the raw write token for worldName, minting it on first
// use. Pods converge on the broker Secret's first committed record, then
// make the world's tokens.toml hold its hash under the stable label.
func (s *WorldWriteTokenStore) Provision(ctx context.Context, worldName string) (string, error) {
	if tok, ok := s.Get(worldName); ok {
		return tok, nil
	}
	world := core.LookupWorld(s.cfg.Registry(), worldName)
	if world == nil {
		// The WorldPool's sentinel, so federation and graph treat an
		// unknown world the same at dispatch and at provision.
		return "", &errWorldNotFound{worldName: worldName}
	}

	mint := storage.TokenMint{Label: worldWriteTokenLabel(worldName), Paths: world.DefaultToken.Paths}
	record, err := storage.EnsureTokenRecord(ctx, s.store, core.WorldWriteTokenRef(s.cfg, worldName), mint)
	if err != nil {
		return "", err
	}
	if err := storage.SyncWorldHash(ctx, s.store, world, &record); err != nil {
		return "", err
	}

	s.mu.Lock()
	s.cache[worldName] = record.RawToken
	s.mu.Unlock()
	return record.RawToken, nil
}
