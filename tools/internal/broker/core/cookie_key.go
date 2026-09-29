package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// cookieKeyBytes is the size of a generated state cookie HMAC key;
// minCookieKeyBytes the shortest accepted, below which the HMAC is brute-forceable.
const (
	cookieKeyBytes    = 32
	minCookieKeyBytes = 16
)

// DecodeCookieKey decodes a base64 state cookie key and enforces its minimum length.
func DecodeCookieKey(b64Key string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return nil, fmt.Errorf("decode cookie key: %w", err)
	}
	if len(key) < minCookieKeyBytes {
		return nil, fmt.Errorf("cookie key too short (%d bytes, need ≥%d)", len(key), minCookieKeyBytes)
	}
	return key, nil
}

// CookieKeyResult is the persisted base64 state cookie key and whether this
// process generated it.
type CookieKeyResult struct {
	Key       string
	Generated bool
}

// EnsureCookieKey returns the persisted state cookie key, generating one when
// absent. Create-only, so racing replicas converge; rotation is delete + restart.
func EnsureCookieKey(ctx context.Context, store SecretStore, ref SecretRef) (CookieKeyResult, error) {
	key, generated, err := EnsureCreated(ctx, store, ref, generateCookieKey)
	if err != nil {
		return CookieKeyResult{}, fmt.Errorf("ensure cookie key %s: %w", ref, err)
	}
	if _, err := DecodeCookieKey(string(key)); err != nil {
		return CookieKeyResult{}, fmt.Errorf("cookie key %s is invalid: %w", ref, err)
	}
	return CookieKeyResult{Key: string(key), Generated: generated}, nil
}

// generateCookieKey is cookieKeyBytes of randomness, base64 as NewSigner reads it.
func generateCookieKey() ([]byte, error) {
	raw := make([]byte, cookieKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate cookie key: %w", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(raw)), nil
}
