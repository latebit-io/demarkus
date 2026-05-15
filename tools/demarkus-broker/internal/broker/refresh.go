package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
)

// RefreshTokensSecretKey is the key inside the broker-namespace
// refresh-tokens Secret holding the JSON map of sha256(refresh_token)
// → record. Stable so operators can grep the rendered Secret without
// chasing a version-specific key name.
const RefreshTokensSecretKey = "refresh_tokens.json"

// defaultRefreshTokensSecret is the production-safe Secret name
// applied when ServerConfig.RefreshTokensSecret is omitted. Keeps
// test configs and dev deployments self-consistent without forcing
// every operator to set the field.
const defaultRefreshTokensSecret = "demarkus-broker-refresh-tokens"

// defaultRefreshTokenTTL is the production-safe lifetime applied
// when ServerConfig.RefreshTokenTTL is omitted. 90 days matches the
// industry-standard refresh lifetime (Google / Auth0 / Okta defaults
// sit in the 60-180d range). Operators tighten via config.
const defaultRefreshTokenTTL = 90 * 24 * time.Hour

// refreshTokenBytes is the raw entropy size of an opaque refresh
// token before hex encoding. 32 bytes (256 bits) is the standard
// posture for opaque OAuth2 refresh tokens; together with the
// sha256-on-disk hash, a Secret-data exfiltration cannot be turned
// into a mint without solving sha256 pre-image on 256-bit input —
// infeasible by construction.
const refreshTokenBytes = 32

// ErrRefreshTokenInvalid is the sentinel for every "this refresh
// token will not mint anything" condition: unknown, tampered,
// expired, or revoked. Callers translate to RFC 6749 §5.2
// invalid_grant on the /device/token refresh branch. One sentinel
// rather than four so the broker does not leak which failure mode
// triggered the rejection — a tampered token and an expired token
// must be indistinguishable to a polling client.
var ErrRefreshTokenInvalid = errors.New("broker: refresh token invalid or expired")

// refreshTokenRecord is the broker-side row stored in the
// refresh-tokens Secret. The raw token NEVER touches disk — only its
// sha256 hash is the map key, and the record itself holds no token
// material. A Secret-data leak reveals identity (email + sub) and
// timestamps but cannot be turned into a working refresh token
// without breaking sha256 pre-image resistance.
//
// Claims is the verified subset of the device-flow ID token captured
// at /auth/callback time. PR4's refresh branch (Step 3) reconstructs
// the response from this snapshot, re-signing a fresh id_token with
// the broker's key so the bearer-validation downstream sees a token
// with `exp` in the future. See the parent plan §"Out of Scope" for
// why the broker does NOT round-trip back to the IdP on refresh.
type refreshTokenRecord struct {
	// KeyHash is sha256-hex of the raw token. Stored alongside the
	// record (and used as the map key) so log lines and JSON dumps
	// have a stable, non-reversible identifier independent of the
	// containing map key. Duplicate-by-design.
	KeyHash    string    `json:"keyHash"`
	Claims     Claims    `json:"claims"`
	IssuedAt   time.Time `json:"issuedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitzero"`
}

// refreshTokens is the on-disk shape of the broker's refresh-tokens
// Secret data[refresh_tokens.json]: a map keyed by sha256 hex.
// Map vs. slice (issuances uses a slice) reflects access pattern —
// every Refresh and Revoke is an O(1) hash lookup; a slice would
// force an O(N) scan on every poll, and a 90-day TTL means N can
// reach several thousand on a busy broker.
type refreshTokens struct {
	Entries map[string]refreshTokenRecord `json:"entries"`
}

// refreshStore is the broker's refresh-token lifecycle layer.
// All state lives in a single broker-namespace Secret; mutations go
// through the shared optimistic-concurrency mutateSecret helper.
// Multi-replica brokers converge under conflict retry — same posture
// as the issuances Secret.
//
// Single-Secret scaling cap is documented in
// /plans/universe-onboarding-pr4.md §Risks ("Secret-storage scaling"):
// ~5000 active records at 90-day TTL before the 1 MiB etcd Secret
// limit becomes the wall. Phase-7 follow-up is a sharded or
// CRD-backed store; the API surface here stays stable across that
// migration.
type refreshStore struct {
	cfg    *Config
	k8s    kubernetes.Interface
	clock  func() time.Time
	randFn func([]byte) (int, error)
}

// newRefreshStore wires a refreshStore with real time.Now and
// crypto/rand. Tests override clock and randFn on the returned
// struct for deterministic behavior — same pattern as NewIssuer.
func newRefreshStore(cfg *Config, k8s kubernetes.Interface) *refreshStore {
	return &refreshStore{
		cfg:    cfg,
		k8s:    k8s,
		clock:  time.Now,
		randFn: rand.Read,
	}
}

// Issue mints a fresh opaque refresh token, stashes its hashed record
// in the Secret, and returns the raw token for the caller to hand
// back to the polling client. ttl must be > 0; callers (the
// /device/token success branch in Step 2) pass
// ServerConfig.RefreshTokenTTL.
//
// The raw token is hex-encoded 32-byte randomness (64 chars). The
// caller is responsible for treating it as bearer credential material
// — no logging, no retries that would surface it in error messages.
func (s *refreshStore) Issue(ctx context.Context, claims Claims, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("refresh ttl must be > 0 (got %s)", ttl)
	}
	buf := make([]byte, refreshTokenBytes)
	if _, err := s.randFn(buf); err != nil {
		return "", fmt.Errorf("generate refresh token entropy: %w", err)
	}
	raw := hex.EncodeToString(buf)
	keyHash := hashRefreshToken(raw)
	now := s.clock()
	record := refreshTokenRecord{
		KeyHash:   keyHash,
		Claims:    claims,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
	err := mutateSecret(ctx, s.k8s, s.cfg.Server.BrokerNamespace, s.cfg.Server.RefreshTokensSecret, RefreshTokensSecretKey, func(existing []byte) ([]byte, error) {
		store, err := decodeRefreshTokens(existing)
		if err != nil {
			return nil, err
		}
		// sha256 collision on a 32-random-byte pre-image is
		// astronomically improbable. The check exists so a
		// hypothetical collision fails loudly rather than silently
		// rotating one record over another.
		if _, exists := store.Entries[keyHash]; exists {
			return nil, fmt.Errorf("refresh token hash collision on %s", keyHash[:8])
		}
		store.Entries[keyHash] = record
		return json.Marshal(store)
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// Refresh validates the supplied raw token against the store. On
// success, updates the record's LastUsedAt timestamp (one
// extra Secret RMW per call — acceptable for the human-scale rate of
// refresh) and returns the stored record so the caller can rebuild a
// response from the cached Claims. On any "not minting" condition
// returns ErrRefreshTokenInvalid; I/O failures propagate.
func (s *refreshStore) Refresh(ctx context.Context, rawToken string) (refreshTokenRecord, error) {
	if rawToken == "" {
		return refreshTokenRecord{}, ErrRefreshTokenInvalid
	}
	keyHash := hashRefreshToken(rawToken)
	var out refreshTokenRecord
	err := mutateSecret(ctx, s.k8s, s.cfg.Server.BrokerNamespace, s.cfg.Server.RefreshTokensSecret, RefreshTokensSecretKey, func(existing []byte) ([]byte, error) {
		// Reset on each retry — a conflict-driven re-run must not
		// leak state from the prior closure invocation.
		out = refreshTokenRecord{}
		store, err := decodeRefreshTokens(existing)
		if err != nil {
			return nil, err
		}
		record, ok := store.Entries[keyHash]
		if !ok {
			return nil, ErrRefreshTokenInvalid
		}
		now := s.clock()
		if !record.ExpiresAt.IsZero() && now.After(record.ExpiresAt) {
			return nil, ErrRefreshTokenInvalid
		}
		record.LastUsedAt = now
		store.Entries[keyHash] = record
		out = record
		return json.Marshal(store)
	})
	if err != nil {
		return refreshTokenRecord{}, err
	}
	return out, nil
}

// Revoke drops a token from the store. Idempotent per RFC 7009 §2.2:
// revoking an unknown or already-revoked token is not an error. An
// absent Secret is the "store is empty" case and also a no-op.
func (s *refreshStore) Revoke(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	keyHash := hashRefreshToken(rawToken)
	return mutateSecret(ctx, s.k8s, s.cfg.Server.BrokerNamespace, s.cfg.Server.RefreshTokensSecret, RefreshTokensSecretKey, func(existing []byte) ([]byte, error) {
		if len(existing) == 0 {
			return existing, nil
		}
		store, err := decodeRefreshTokens(existing)
		if err != nil {
			return nil, err
		}
		if _, ok := store.Entries[keyHash]; !ok {
			return existing, nil
		}
		delete(store.Entries, keyHash)
		return json.Marshal(store)
	})
}

// Sweep removes every expired entry in one pass. Returns the count
// of records swept so the caller can log it. Absent Secret and empty
// map are both no-ops returning (0, nil). Designed to be called from
// the existing Sweeper's per-tick loop alongside the issuances
// sweep — Step 5 wires it.
func (s *refreshStore) Sweep(ctx context.Context) (int, error) {
	var swept int
	err := mutateSecret(ctx, s.k8s, s.cfg.Server.BrokerNamespace, s.cfg.Server.RefreshTokensSecret, RefreshTokensSecretKey, func(existing []byte) ([]byte, error) {
		// Reset on each retry — same rationale as Refresh.
		swept = 0
		if len(existing) == 0 {
			return existing, nil
		}
		store, err := decodeRefreshTokens(existing)
		if err != nil {
			return nil, err
		}
		if len(store.Entries) == 0 {
			return existing, nil
		}
		now := s.clock()
		for k := range store.Entries {
			rec := store.Entries[k]
			if !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt) {
				delete(store.Entries, k)
				swept++
			}
		}
		if swept == 0 {
			return existing, nil
		}
		return json.Marshal(store)
	})
	return swept, err
}

// hashRefreshToken returns the sha256-hex of the raw token. Used as
// the Secret-map key and the record's KeyHash field. sha256 is the
// canonical opaque-token hashing choice (matches RFC 9068's
// recommendation for `at_hash`-style derivations) and ships in the
// stdlib — no third-party crypto dependency.
func hashRefreshToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// decodeRefreshTokens parses the on-disk Secret body. An empty input
// (Secret-not-yet-created or empty data key) returns an empty
// initialized store so callers can mutate without nil-map panics.
func decodeRefreshTokens(existing []byte) (refreshTokens, error) {
	if len(existing) == 0 {
		return refreshTokens{Entries: make(map[string]refreshTokenRecord)}, nil
	}
	var out refreshTokens
	if err := json.Unmarshal(existing, &out); err != nil {
		return refreshTokens{}, fmt.Errorf("decode refresh tokens: %w", err)
	}
	if out.Entries == nil {
		out.Entries = make(map[string]refreshTokenRecord)
	}
	return out, nil
}
