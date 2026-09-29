package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// cookieKeyBytes is the size of a generated state cookie HMAC key.
const cookieKeyBytes = 32

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
