package broker

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// authCodeIDBytes is the raw entropy of the opaque internal id that
// links a pending /oauth/authorize request to the eventual
// /auth/callback resolution. 32 bytes = 64 hex chars; the id rides
// inside the signed State cookie, never on the wire as a URL
// parameter, so length is not a UX concern.
const authCodeIDBytes = 32

// authCodeBytes is the raw entropy of the authorization code we hand
// the MCP client through the redirect_uri query. 32 bytes encoded as
// base64url-no-pad keeps the URL short while preserving 256 bits of
// entropy — well past brute-force inside the 60-second redemption
// window.
const authCodeBytes = 32

// defaultPendingAuthCodeTTL is the lifetime of a pending grant from
// /oauth/authorize until /auth/callback resolves it. Ten minutes
// covers the worst case of a user fumbling through a corporate IdP
// (MFA prompt, password reset interstitial, etc.) while keeping
// abandoned grants from accumulating.
const defaultPendingAuthCodeTTL = 10 * time.Minute

// defaultAuthCodeTTL is the lifetime of an issued authorization code
// from /auth/callback's redirect until /token redeems it. OAuth 2.1
// §4.1.2 advises ≤ 60 seconds; we use the tight bound because the
// only legitimate consumer is a localhost callback handler that
// turns around and posts to /token immediately.
const defaultAuthCodeTTL = 60 * time.Second

// errAuthCodePendingNotFound is returned by LookupPending and Bind
// when the supplied authCodeID is unknown or its pending entry
// expired. The callback handler maps it to a redirect with
// `error=invalid_request` per RFC 6749 §4.1.2.1 (the original
// authorize request is no longer valid).
var errAuthCodePendingNotFound = errors.New("auth code pending entry not found")

// errAuthCodeNotFound is returned by Redeem when the code is unknown,
// already redeemed, or expired. The /token handler maps it to
// `invalid_grant` per RFC 6749 §5.2; we deliberately collapse
// "unknown", "redeemed", and "expired" into one sentinel so the
// surface does not leak which axis failed.
var errAuthCodeNotFound = errors.New("auth code not found")

// errAuthCodeClientMismatch is returned when the client_id presented
// at /token does not match the one captured at /oauth/authorize. RFC
// 6749 §4.1.3 requires this binding; the /token handler maps it to
// `invalid_grant` (same surface as errAuthCodeNotFound to avoid
// leaking which check failed).
var errAuthCodeClientMismatch = errors.New("auth code client_id mismatch")

// errAuthCodeRedirectMismatch is returned when the redirect_uri
// presented at /token does not byte-for-byte match the one captured
// at /oauth/authorize. RFC 6749 §4.1.3 binding; mapped to
// `invalid_grant` at the HTTP layer.
var errAuthCodeRedirectMismatch = errors.New("auth code redirect_uri mismatch")

// errAuthCodePKCEFailed is returned when SHA-256(code_verifier) does
// not match the stored code_challenge. RFC 7636 §4.6 binding; the
// /token handler maps to `invalid_grant`. Comparison is
// constant-time to avoid a timing oracle on the challenge value.
var errAuthCodePKCEFailed = errors.New("auth code PKCE verification failed")

// AuthCodeRequest is the subset of /oauth/authorize parameters the
// store retains across the IdP round-trip. The handler validates
// shape (response_type, code_challenge_method, loopback redirect)
// before constructing the request; this struct is the cleaned form
// the store remembers.
//
// ClientState is the OAuth `state` parameter the MCP client supplied
// — the broker passes it through verbatim on the redirect to the
// client's redirect_uri so the client can defend against its own
// callback CSRF. Distinct from the broker's signed State cookie,
// which protects the broker ↔ IdP leg.
type AuthCodeRequest struct {
	ClientID            string
	RedirectURI         string
	ClientState         string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// PendingAuthCode is what LookupPending returns: the original
// /oauth/authorize request plus the absolute expiry the callback
// handler enforces. Returned by value so callers cannot mutate the
// store's copy outside the mutex.
type PendingAuthCode struct {
	AuthCodeRequest
	ExpiresAt time.Time
}

// authCodeEntry is the post-callback state — the issued code's
// binding to the IdP exchange result plus the original request's
// PKCE + client/redirect for re-check at /token. Owned by the store
// mutex; never returned by pointer.
type authCodeEntry struct {
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	Exchange            ExchangeResult
	ExpiresAt           time.Time
}

// authCodeStore is the in-memory state for all in-flight
// authorization-code grants on this broker replica. Same
// single-broker invariant as deviceStore: process memory only,
// dropped on restart. Acceptable because the pending TTL is short
// (10m) and the issued-code TTL is shorter still (60s); the
// redirect-flow window comfortably fits inside a pod restart's
// readiness gate.
type authCodeStore struct {
	mu sync.Mutex

	pending map[string]*PendingAuthCode // authCodeID → pending request
	codes   map[string]*authCodeEntry   // issued code → bound entry

	clock      func() time.Time
	pendingTTL time.Duration
	codeTTL    time.Duration
}

// newAuthCodeStore builds a fresh in-memory store. pendingTTL is the
// /oauth/authorize → /auth/callback window; codeTTL is the
// /auth/callback → /token window. clock is injected so tests can
// drive expiry deterministically.
func newAuthCodeStore(clock func() time.Time, pendingTTL, codeTTL time.Duration) *authCodeStore {
	if clock == nil {
		clock = time.Now
	}
	if pendingTTL <= 0 {
		pendingTTL = defaultPendingAuthCodeTTL
	}
	if codeTTL <= 0 {
		codeTTL = defaultAuthCodeTTL
	}
	return &authCodeStore{
		pending:    make(map[string]*PendingAuthCode),
		codes:      make(map[string]*authCodeEntry),
		clock:      clock,
		pendingTTL: pendingTTL,
		codeTTL:    codeTTL,
	}
}

// Begin records a new pending /oauth/authorize grant and returns the
// opaque internal id the handler pins inside the signed State cookie.
// The id never leaves the broker — it is not the authorization code
// the client eventually sees — so its sole job is to link the
// callback back to the original request. Takes a pointer to
// AuthCodeRequest to avoid copying the ~100-byte struct through the
// call site; the store dereferences into its own copy under the lock.
func (s *authCodeStore) Begin(req *AuthCodeRequest) (string, error) {
	id, err := newAuthCodeID()
	if err != nil {
		return "", fmt.Errorf("auth code id: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[id] = &PendingAuthCode{
		AuthCodeRequest: *req,
		ExpiresAt:       s.clock().Add(s.pendingTTL),
	}
	return id, nil
}

// LookupPending returns a copy of the pending grant for the supplied
// id, or false when the id is unknown or expired. Unlike Bind it
// does not mutate the store — callbacks can peek (e.g. for logging)
// without consuming the entry.
func (s *authCodeStore) LookupPending(id string) (PendingAuthCode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	if !ok {
		return PendingAuthCode{}, false
	}
	if s.clock().After(p.ExpiresAt) {
		return PendingAuthCode{}, false
	}
	return *p, true
}

// Bind transitions a pending grant into an issued authorization code.
// On success it returns the freshly-minted code plus a copy of the
// pending request so the callback handler can build the redirect URL
// (it needs the client's redirect_uri + client_state). The pending
// entry is removed atomically — a second Bind on the same id returns
// errAuthCodePendingNotFound.
//
// Takes a pointer to ExchangeResult to avoid copying ~120 bytes
// through the call site; same pattern as deviceStore.Bind.
//
// On expired pending: errAuthCodePendingNotFound (the entry is also
// dropped so a follow-up sweep does not have to re-find it).
func (s *authCodeStore) Bind(id string, exchange *ExchangeResult) (string, PendingAuthCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	if !ok {
		return "", PendingAuthCode{}, errAuthCodePendingNotFound
	}
	now := s.clock()
	if now.After(p.ExpiresAt) {
		delete(s.pending, id)
		return "", PendingAuthCode{}, errAuthCodePendingNotFound
	}
	code, err := newAuthCode()
	if err != nil {
		return "", PendingAuthCode{}, fmt.Errorf("auth code: %w", err)
	}
	s.codes[code] = &authCodeEntry{
		ClientID:            p.ClientID,
		RedirectURI:         p.RedirectURI,
		CodeChallenge:       p.CodeChallenge,
		CodeChallengeMethod: p.CodeChallengeMethod,
		Exchange:            *exchange,
		ExpiresAt:           now.Add(s.codeTTL),
	}
	out := *p
	delete(s.pending, id)
	return code, out, nil
}

// Redeem consumes an issued authorization code. The handler at /token
// drives this from a `grant_type=authorization_code` request: code +
// client_id + redirect_uri + code_verifier. On success the entry is
// removed (one-shot — a replay returns errAuthCodeNotFound) and the
// stored IdP exchange result is returned for token minting.
//
// Validation order is deliberate: existence + expiry → client_id →
// redirect_uri → PKCE. Each axis maps to a distinct sentinel so
// internal logs name the failure without the handler having to
// rebuild the reason from context — handler still maps every error
// to `invalid_grant` on the wire.
//
// Consumption semantics: only successful redemption and expiry
// delete the code. errAuthCodeClientMismatch /
// errAuthCodeRedirectMismatch / errAuthCodePKCEFailed leave the
// entry in place so a transient client misconfig — wrong client_id
// passed in a retry, race between two callback ports, momentarily
// wrong code_verifier — does not burn the user's flow inside the
// 60-second window. The 256-bit code entropy and the PKCE binding
// keep brute-force probes infeasible in that window. The "transient
// client misconfig" subtest pins this contract. The HTTP handler in
// oauth_authorize.go is the right place to emit a structured
// debug log on validation failure (request context lives there,
// not in the store).
func (s *authCodeStore) Redeem(code, clientID, redirectURI, codeVerifier string) (ExchangeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.codes[code]
	if !ok {
		return ExchangeResult{}, errAuthCodeNotFound
	}
	if s.clock().After(entry.ExpiresAt) {
		delete(s.codes, code)
		return ExchangeResult{}, errAuthCodeNotFound
	}
	if subtle.ConstantTimeCompare([]byte(entry.ClientID), []byte(clientID)) != 1 {
		return ExchangeResult{}, errAuthCodeClientMismatch
	}
	if subtle.ConstantTimeCompare([]byte(entry.RedirectURI), []byte(redirectURI)) != 1 {
		return ExchangeResult{}, errAuthCodeRedirectMismatch
	}
	if !verifyPKCE(entry.CodeChallenge, entry.CodeChallengeMethod, codeVerifier) {
		return ExchangeResult{}, errAuthCodePKCEFailed
	}
	out := entry.Exchange
	delete(s.codes, code)
	return out, nil
}

// Sweep evicts expired pending and issued entries. Called by the
// janitor; safe to call concurrently with all other store methods.
// Expired-but-not-yet-redeemed codes are already rejected by Redeem
// (the lazy expiry check above) — Sweep just keeps the map from
// growing unboundedly under abandoned flows.
func (s *authCodeStore) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	for id, p := range s.pending {
		if now.After(p.ExpiresAt) {
			delete(s.pending, id)
		}
	}
	for code, entry := range s.codes {
		if now.After(entry.ExpiresAt) {
			delete(s.codes, code)
		}
	}
}

// verifyPKCE returns true iff method+verifier matches the stored
// challenge. The broker accepts S256 only — `plain` is excluded by
// the /oauth/authorize handler before Begin is called, so a
// non-S256 method here is a programming error and we return false
// rather than try to handle it (defense-in-depth — a future
// refactor that accidentally lets `plain` through would still fail
// closed at Redeem time).
func verifyPKCE(challenge, method, verifier string) bool {
	if method != "S256" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(challenge), []byte(computed)) == 1
}

// newAuthCodeID returns a fresh opaque internal id from crypto/rand.
// Hex-encoded for symmetry with newDeviceCode — the id never appears
// in a URL, so encoding choice is cosmetic.
func newAuthCodeID() (string, error) {
	buf := make([]byte, authCodeIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// newAuthCode returns a fresh authorization code from crypto/rand.
// base64url-no-pad keeps the redirect URL short; the code is opaque
// to every caller so encoding choice is internal.
func newAuthCode() (string, error) {
	buf := make([]byte, authCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
