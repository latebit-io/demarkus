package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Claims is the subset of OIDC id_token claims the broker consumes. Groups
// is nil when the IdP surfaces none. HD is Google's hosted domain, asserted
// by Google's signature, which is why the domain gate keys on it, not email.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Groups        []string
	HD            string
}

// ctxKey scopes context keys to this package.
type ctxKey int

// ctxClaimsKey holds the verified Claims of an authenticated request, set
// by the auth middleware and read by handlers and the subject limiter.
const ctxClaimsKey ctxKey = iota

// CtxWithClaims attaches the verified claims so every handler downstream
// shares one authentication result.
func CtxWithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, ctxClaimsKey, c)
}

// ClaimsFromCtx returns the verified claims; false means the request did
// not pass the auth middleware, which callers treat as an invariant break.
func ClaimsFromCtx(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxClaimsKey).(*Claims)
	return c, ok
}

// CanonicalEmail is the one identity normalization: trimmed and lowercased.
func CanonicalEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// IdentityKey is the stable provisioning identity: the configured issuer plus
// subject. A token's own iss varies between IdP and broker signed paths, and
// email is display identity that may change.
func IdentityKey(issuer, subject string) string {
	return issuer + "|" + subject
}

// HashSubject is a short, stable, non reversible log fingerprint; the raw
// subject often embeds the user's email or IdP id.
func HashSubject(subject string) string {
	if subject == "" {
		return ""
	}
	h := sha256.Sum256([]byte(subject))
	return "sha256-" + hex.EncodeToString(h[:4])
}
