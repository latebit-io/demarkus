package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

// RFC 7591 dynamic client registrations with persisted redirect URIs:
// MCP hosts register their https callbacks and the authorize leg
// trusts exactly what was recorded here.

// defaultDynamicClientsSecret names the broker-namespace Secret
// holding the registration map when the operator does not override it.
const defaultDynamicClientsSecret = "demarkus-broker-dynamic-clients"

// DynamicClientsSecretKey is the data key inside the Secret.
const DynamicClientsSecretKey = "dynamic-clients.json"

// maxDynamicClients caps the registration map. Registration is
// anonymous-open (RFC 7591 §2), so without a cap the Secret is an
// unauthenticated storage-growth surface.
const maxDynamicClients = 2000

// maxDynamicClientsBytes caps the SERIALIZED map, well under the 1MiB
// Kubernetes Secret limit: record count alone cannot bound bytes.
const maxDynamicClientsBytes = 512 << 10

// Per-registration shape bounds; count and byte caps only hold when a
// single anonymous registration cannot be arbitrarily large.
const (
	maxRedirectURIsPerClient = 8
	maxRedirectURILen        = 512
	maxClientNameLen         = 128
)

// dynamicClientTTL expires registrations. MCP hosts re-register
// cheaply on their next connect, so expiry only costs a re-dance.
const dynamicClientTTL = 90 * 24 * time.Hour

// dynamicClientRecord is one registration's persisted state.
type dynamicClientRecord struct {
	RedirectURIs []string  `json:"redirectURIs"`
	ClientName   string    `json:"clientName,omitempty"`
	Created      time.Time `json:"created"`
}

func (r *dynamicClientRecord) expired(now time.Time) bool {
	return now.Sub(r.Created) > dynamicClientTTL
}

func (r *dynamicClientRecord) allowsRedirect(uri string) bool {
	return slices.Contains(r.RedirectURIs, uri)
}

// DynamicClientStore persists RFC 7591 registrations in one Secret,
// sharing the refresh-store pattern: optimistic-concurrency Mutate,
// prune-on-write expiry, survives restarts and replicas.
type DynamicClientStore struct {
	cfg   *Config
	store SecretStore
	clock func() time.Time
}

// NewDynamicClientStore builds the store over the broker's SecretStore.
func NewDynamicClientStore(cfg *Config, store SecretStore) *DynamicClientStore {
	return &DynamicClientStore{cfg: cfg, store: store, clock: time.Now}
}

func decodeDynamicClients(existing []byte) (map[string]dynamicClientRecord, error) {
	clients := map[string]dynamicClientRecord{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &clients); err != nil {
			return nil, fmt.Errorf("broker: decode dynamic-clients state: %w", err)
		}
	}
	return clients, nil
}

// Register persists a new registration under clientID.
func (s *DynamicClientStore) Register(ctx context.Context, clientID string, redirectURIs []string, name string) error {
	now := s.clock().UTC()
	return s.store.Mutate(ctx, s.cfg.dynamicClientsRef(), func(existing []byte) ([]byte, error) {
		clients, err := decodeDynamicClients(existing)
		if err != nil {
			return nil, err
		}
		for id, record := range clients {
			if record.expired(now) {
				delete(clients, id)
			}
		}
		clients[clientID] = dynamicClientRecord{
			RedirectURIs: redirectURIs,
			ClientName:   name,
			Created:      now,
		}
		// Over capacity (count OR serialized bytes), evict oldest
		// rather than refuse: refusal lets anonymous churn deny new
		// hosts; an evicted live host re-registers on next connect.
		for {
			encoded, encErr := json.Marshal(clients)
			if encErr != nil {
				return nil, encErr
			}
			if len(clients) <= maxDynamicClients && len(encoded) <= maxDynamicClientsBytes {
				return encoded, nil
			}
			oldestID := ""
			var oldest time.Time
			for id, record := range clients {
				if id == clientID {
					continue // never evict the registration being added
				}
				if oldestID == "" || record.Created.Before(oldest) {
					oldestID, oldest = id, record.Created
				}
			}
			if oldestID == "" {
				return encoded, nil // only the new record remains
			}
			delete(clients, oldestID)
		}
	})
}

// Lookup returns the registration for clientID when present and not
// expired. Read-only: the Mutate no-op write suppression means no
// Secret update is issued.
func (s *DynamicClientStore) Lookup(ctx context.Context, clientID string) (dynamicClientRecord, bool, error) {
	var record dynamicClientRecord
	found := false
	err := s.store.Mutate(ctx, s.cfg.dynamicClientsRef(), func(existing []byte) ([]byte, error) {
		clients, decodeErr := decodeDynamicClients(existing)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if r, ok := clients[clientID]; ok && !r.expired(s.clock().UTC()) {
			record, found = r, true
		}
		return existing, nil
	})
	if err != nil {
		return dynamicClientRecord{}, false, err
	}
	return record, found, nil
}

// nativeRedirectURIs: exact-match allowlist of private-use scheme
// callbacks (RFC 8252 §7.1) MCP hosts register via DCR. Whole URIs,
// never schemes; rationale in ADR 0011.
var nativeRedirectURIs = map[string]struct{}{
	"cursor://anysphere.cursor-mcp/oauth/callback": {},
}

func isNativeRedirectURI(raw string) bool {
	_, ok := nativeRedirectURIs[raw]
	return ok
}

// validateClientRedirectURI accepts only trustable redirect shapes:
// http loopback, an allowlisted native-scheme callback, or the
// webClients https shape (absolute, no userinfo, no fragment).
func validateClientRedirectURI(raw string) error {
	if isLoopbackRedirectURI(raw) || isNativeRedirectURI(raw) {
		return nil
	}
	return validateWebRedirectURI(raw)
}

func (c *Config) dynamicClientsRef() SecretRef {
	ref := SecretRef{
		Namespace: c.Server.BrokerNamespace,
		Name:      c.Server.DynamicClientsSecret,
		Key:       DynamicClientsSecretKey,
	}
	if c.fileBackend() {
		ref.Path = filepath.Join(c.Storage.Dir, DynamicClientsSecretKey)
	}
	return ref
}
