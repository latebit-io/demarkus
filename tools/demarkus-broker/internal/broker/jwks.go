package broker

import (
	"encoding/json"
	"net/http"

	"github.com/go-jose/go-jose/v4"
)

// jwksHandler serves the broker's JSON Web Key Set at
// /.well-known/jwks.json. The endpoint is public and unauthenticated
// (matching the OIDC discovery doc's posture) so any OAuth/OIDC
// client library can fetch the verification key for broker-signed
// id_tokens. PR4 publishes exactly one key; rotation (multiple
// concurrent keys with overlapping kids) is a phase-7 follow-up.
type jwksHandler struct {
	signer *IDTokenSigner
	body   []byte
}

// newJWKSHandler renders the JWKS once at construction. The signer's
// kid is stable across the broker's lifetime (no rotation in PR4),
// so a one-time render avoids any per-request marshaling overhead.
// Re-rendering on signer change is a phase-7 concern.
func newJWKSHandler(signer *IDTokenSigner) (*jwksHandler, error) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{signer.PublicJWK()}}
	body, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return nil, err
	}
	return &jwksHandler{signer: signer, body: body}, nil
}

// ServeHTTP writes the rendered JWKS body. Method enforcement is
// already handled by the mux's `GET /.well-known/jwks.json` pattern,
// so the handler itself only writes the body.
func (h *jwksHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Cache-Control matches the discovery doc's TTL window (300s,
	// kept as a literal because Cache-Control values are part of
	// the HTTP wire contract — coupling to defaultDiscoveryTTL via
	// a computed value would silently drift if either side changed).
	// Public (not private) because the JWKS is not user-scoped.
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(h.body)
}
