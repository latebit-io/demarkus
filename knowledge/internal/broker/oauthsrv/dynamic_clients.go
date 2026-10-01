package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/server/blob"
)

// RFC 7591 dynamic client registrations with persisted redirect URIs:
// MCP hosts register their https callbacks and the authorize leg
// trusts exactly what was recorded here.

// Per-registration shape bounds: registration is anonymous (RFC 7591 §2),
// so one record must stay small; the IP rate limit and TTL bound the count.
const (
	maxRedirectURIsPerClient = 8
	maxRedirectURILen        = 512
	maxClientNameLen         = 128
)

// dynamicClientTTL expires registrations. MCP hosts re-register
// cheaply on their next connect, so expiry only costs a re-dance.
const dynamicClientTTL = 90 * 24 * time.Hour

// clientsPrefix holds one object per registration in the state bucket.
const clientsPrefix = "clients/"

// clientIDRE is the shape of an issued client_id, checked before a caller's
// value becomes an object name.
var clientIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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

// DynamicClientStore keeps RFC 7591 registrations in the state bucket, one
// object per client_id; the sweep deletes them after dynamicClientTTL.
type DynamicClientStore struct {
	dir   stateDir
	clock func() time.Time
}

// NewDynamicClientStore builds the store over the state bucket.
func NewDynamicClientStore(objects blob.Store) *DynamicClientStore {
	return &DynamicClientStore{dir: stateDir{objects: objects, prefix: clientsPrefix}, clock: time.Now}
}

// Register persists a new registration under clientID.
func (s *DynamicClientStore) Register(ctx context.Context, clientID string, redirectURIs []string, name string) error {
	if !clientIDRE.MatchString(clientID) {
		return fmt.Errorf("register: malformed client_id")
	}
	record := dynamicClientRecord{RedirectURIs: redirectURIs, ClientName: name, Created: s.clock().UTC()}
	if err := s.dir.create(ctx, clientID, record); err != nil {
		return fmt.Errorf("register %s: %w", clientID, err)
	}
	return nil
}

// Lookup returns the live registration for clientID; found is false for an
// unknown, malformed or expired one.
func (s *DynamicClientStore) Lookup(ctx context.Context, clientID string) (record dynamicClientRecord, found bool, err error) {
	if !clientIDRE.MatchString(clientID) {
		return dynamicClientRecord{}, false, nil
	}
	_, err = s.dir.get(ctx, clientID, &record)
	if errors.Is(err, blob.ErrNotFound) {
		return dynamicClientRecord{}, false, nil
	}
	if err != nil {
		return dynamicClientRecord{}, false, err
	}
	if record.expired(s.clock().UTC()) {
		return dynamicClientRecord{}, false, nil
	}
	return record, true, nil
}

// Sweep deletes expired registrations, reading only objects older than the
// TTL: a registration is never rewritten, so younger ones are live.
func (s *DynamicClientStore) Sweep(ctx context.Context) (int, error) {
	now := s.clock().UTC()
	return s.dir.sweep(ctx, func(attrs blob.Attributes) (bool, blob.Generation, error) {
		if !attrs.Modified.Before(now.Add(-dynamicClientTTL)) {
			return false, 0, nil
		}
		var record dynamicClientRecord
		generation, err := s.dir.get(ctx, attrs.Key[len(clientsPrefix):], &record)
		return record.expired(now), generation, err
	})
}

// ImportSecret copies live registrations from the pre-bucket Secret, then
// empties it. Idempotent, so an interrupted import reruns at next start.
func (s *DynamicClientStore) ImportSecret(ctx context.Context, secrets core.SecretStore, ref core.SecretRef) (int, error) {
	imported := 0
	err := secrets.Mutate(ctx, ref, func(existing []byte) ([]byte, error) {
		imported = 0
		if len(existing) == 0 {
			return existing, nil
		}
		clients := map[string]dynamicClientRecord{}
		if err := json.Unmarshal(existing, &clients); err != nil {
			return nil, fmt.Errorf("decode %s: %w", ref.Name, err)
		}
		now := s.clock().UTC()
		for id := range clients {
			record := clients[id]
			if record.expired(now) || !clientIDRE.MatchString(id) {
				continue
			}
			err := s.dir.create(ctx, id, record)
			if err != nil && !errors.Is(err, errRecordExists) {
				return nil, err
			}
			imported++
		}
		return nil, nil
	})
	return imported, err
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
	return core.ValidateWebRedirectURI(raw)
}
