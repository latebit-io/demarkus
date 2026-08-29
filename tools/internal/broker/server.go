package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// stateCookieName is the name of the signed cookie holding the OIDC state
// nonce + expiry. The browser sends it back on /auth/callback so the
// broker can cross-check against the OAuth `state` query parameter
// (defense against login-CSRF / IdP-mixup).
const stateCookieName = "broker_oidc_state"

// Server is the broker's HTTP layer. It depends on the Verifier plus a
// kubernetes client for the Secret-backed stores; tests pass a
// fakeVerifier and a fake clientset and exercise every route end-to-end
// without network or kube.
type Server struct {
	cfg       *Config
	signer    *Signer
	verifier  Verifier
	discovery *Discovery
	log       *slog.Logger
	clock     func() time.Time

	// deviceStore holds in-flight RFC 8628 device-flow grants. Created
	// lazily by NewServer (single-broker invariant per plan §"Single
	// broker only for now"); state lives in process memory and a
	// broker restart drops it. The handlers in device.go own the
	// HTTP surface; the store owns the state machine.
	deviceStore *deviceStore

	// authCodeStore holds in-flight RFC 6749 authorization-code grants
	// for MCP clients that do not pivot to device flow (Claude Code's
	// MCP SDK as of 2026-05). Same single-broker invariant as
	// deviceStore — process-memory only, dropped on restart. The
	// handlers in oauth_authorize.go own the HTTP surface; the store
	// owns the pending → code → exchange state machine.
	authCodeStore *authCodeStore

	// refreshStore owns the persisted map of sha256(refresh_token) →
	// record; survives broker restarts unlike deviceStore. NewServer
	// injects one SecretStore shared with the write-token store.
	refreshStore *RefreshStore

	// dynamicClients persists RFC 7591 registrations so the authorize
	// leg can trust an MCP host's registered https redirect URIs.
	dynamicClients *DynamicClientStore

	// idTokenSigner mints broker-signed id_tokens on the
	// /device/token refresh-grant path and serves its public key at
	// /.well-known/jwks.json. May be nil in tests that don't
	// exercise the refresh-grant surface; production wiring always
	// supplies one (NewServer.LoadConfig validates the PEM at
	// startup). Same signer satisfies both sign and verify (broker
	// is the only relying party for its own tokens).
	idTokenSigner *IDTokenSigner

	// jwks holds the pre-rendered JWKS body served at
	// /.well-known/jwks.json. Nil when idTokenSigner is nil;
	// Routes() skips the registration in that case so existing
	// tests that pass nil signers behave unchanged.
	jwks *jwksHandler

	// worldWriteTokens owns the per-world write-token Secrets the
	// MCP gateway dispatches writes with. One long-lived token per
	// world, shared across all writers — SSO + WorldConfig.Allow at
	// the broker is the writer gate; the world only ever sees one
	// token-per-world in its tokens.toml.
	worldWriteTokens *worldWriteTokenStore

	// mcpPool is the production worldPool wired by MCPGateway, held
	// here so CloseMCPGateway can drain pooled QUIC connections at
	// shutdown. Nil for tests that go through MCPGatewayWith (they
	// own their fake dispatcher's lifecycle).
	mcpPool *worldPool

	// provisioner is the dynamic-tenant creator (memory broker with
	// provisioning enabled); nil everywhere else. tenantGate consults it
	// when an authenticated identity resolves to no world.
	provisioner *Provisioner

	// profile is the gateway profile, held so the management API
	// (/me/install, /auth/callback) shares the gateway's tenant scoping
	// from one source of truth. Nil in tests that skip Run.
	profile *GatewayProfile

	// subjectReg is the per-subject limiter for /me/install; loginReg
	// is the per-IP limiter for /auth/login.
	// Either may be nil (Slice C.4 RateLimitConfig.Disabled, or a test
	// that constructs Config{} without rate-limit fields set), in
	// which case the corresponding middleware is a no-op passthrough.
	subjectReg *rateLimitRegistry
	loginReg   *rateLimitRegistry
	// trustForwardedFor mirrors RateLimitConfig.TrustForwardedFor; the
	// flag is hoisted onto Server so the ipRateLimit middleware can
	// read it without re-reaching into Config on every request.
	trustForwardedFor bool
}

// NewServer wires a Server. The caller is responsible for constructing
// the Signer, Verifier, kubernetes client, Discovery, and (optionally)
// IDTokenSigner in advance — keeps this constructor cheap enough for
// tests to call directly. discovery and idTokenSigner are optional:
// tests that don't exercise those surfaces pass nil and Routes() skips
// the corresponding registrations.
//
// When idTokenSigner is non-nil, NewServer composes the supplied
// Verifier with the broker-key verification leg (see
// compositeVerifier in oidc.go) — callers don't have to wrap
// manually, and tests that pass &fakeVerifier{} get broker-signed
// verification "for free" against the supplied signer.
func NewServer(cfg *Config, signer *Signer, verifier Verifier, store SecretStore, discovery *Discovery, idTokenSigner *IDTokenSigner, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	// Discovery overrides jwks_uri to the broker (PR4); a discovery
	// doc that advertises /.well-known/jwks.json with no handler
	// mounted is a broken-by-construction surface — strict OIDC
	// clients will fail at key-discovery time. Production wiring
	// guarantees both are present (LoadConfig requires
	// BrokerSigningKey when validate runs), so this guard is for
	// programming errors at construction. Panic rather than return
	// an error because NewServer's signature is error-less and the
	// invariant is impossible to recover from at runtime.
	if discovery != nil && idTokenSigner == nil {
		panic("broker: NewServer with discovery != nil requires idTokenSigner; otherwise the well-known doc would advertise jwks_uri at a route that is not registered")
	}
	// Device-flow knobs default at construction time so tests that
	// build Config{} directly (skipping LoadConfig validation) still
	// get usable values — keeps the in-process httptest path symmetric
	// with the LoadConfig-validated production path. Same shape for
	// refresh-flow knobs (PR4).
	deviceTTL := cfg.Server.DeviceCodeTTL
	if deviceTTL <= 0 {
		deviceTTL = 10 * time.Minute
	}
	pollInterval := cfg.Server.DevicePollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	if cfg.Server.RefreshTokensSecret == "" {
		cfg.Server.RefreshTokensSecret = defaultRefreshTokensSecret
	}
	if cfg.Server.DynamicClientsSecret == "" {
		cfg.Server.DynamicClientsSecret = defaultDynamicClientsSecret
	}
	if cfg.Server.RefreshTokenTTL <= 0 {
		cfg.Server.RefreshTokenTTL = defaultRefreshTokenTTL
	}
	if cfg.Server.IDTokenTTL <= 0 {
		cfg.Server.IDTokenTTL = defaultIDTokenTTL
	}
	// Wrap the supplied Verifier with the broker-key leg whenever a
	// signer is wired. Callers never see the wrapping — Server.verifier
	// is the composite under the same Verifier interface.
	if idTokenSigner != nil {
		verifier = newCompositeVerifier(verifier, idTokenSigner, cfg.Server.PublicURL)
	}
	clock := time.Now
	s := &Server{
		cfg:               cfg,
		signer:            signer,
		verifier:          verifier,
		discovery:         discovery,
		idTokenSigner:     idTokenSigner,
		log:               log,
		clock:             clock,
		deviceStore:       newDeviceStore(clock, deviceTTL, pollInterval),
		authCodeStore:     newAuthCodeStore(clock, defaultPendingAuthCodeTTL, defaultAuthCodeTTL),
		refreshStore:      NewRefreshStore(cfg, store),
		dynamicClients:    NewDynamicClientStore(cfg, store),
		worldWriteTokens:  newWorldWriteTokenStore(cfg, store),
		trustForwardedFor: cfg.RateLimit.TrustForwardedFor,
	}
	if idTokenSigner != nil {
		jwks, err := newJWKSHandler(idTokenSigner)
		if err != nil {
			// Marshal failure is purely a programming error — the
			// JWK struct cannot legitimately fail to serialize.
			// Panic rather than hide the surface as silently
			// missing; the broker is unsafe to serve refresh
			// grants without a verifiable JWKS.
			panic(fmt.Sprintf("broker: render JWKS at startup: %v", err))
		}
		s.jwks = jwks
	}
	// Build the registries unless the operator disabled the limiter
	// entirely. newRateLimitRegistry returns nil for zero/negative
	// inputs, so tests that construct Config{} without RateLimit set
	// get registry=nil and the middleware passes through cleanly —
	// matching the pre-Slice-C.4 behavior of those tests.
	if !cfg.RateLimit.Disabled {
		s.subjectReg = newRateLimitRegistry(cfg.RateLimit.Tokens.PerMinute, cfg.RateLimit.Tokens.Burst)
		s.loginReg = newRateLimitRegistry(cfg.RateLimit.Login.PerMinute, cfg.RateLimit.Login.Burst)
	}
	return s
}

// Routes returns an http.Handler with the broker's routes registered.
// Routes are mounted on the default ServeMux pattern syntax (Go 1.22+),
// which gives us method + path matching without a router library.
//
// Middleware composition (Slice C.4):
//   - /healthz, /readyz, /auth/callback: no middleware. Health probes
//     must succeed without auth or rate-limiting, and /auth/callback's
//     own state-cookie HMAC gate is the natural rate limit.
//   - /auth/login: ipRateLimit only. Unauthenticated by design — the
//     IP limiter is the only backstop against an attacker hammering
//     the OIDC redirect machinery.
//   - /me/install: requireAuth (verifies bearer, stashes claims on
//     ctx) → then subjectRateLimit (reads claims from ctx, keys on
//     subject hash) → handler (reads claims from ctx via claimsFromCtx).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// /.well-known/openid-configuration: public, unauthenticated, no
	// rate limit. Polling-friendly: device-flow clients fetch it once
	// per /soul-join and never again; intermediaries cache via the
	// Cache-Control header the Discovery handler emits.
	// oauth-authorization-server aliases the same doc at the RFC 8414
	// path; §3.3 requires it on the issuer's origin, which is this listener.
	if s.discovery != nil {
		mux.Handle("GET /.well-known/openid-configuration", s.discovery.Handler())
		mux.Handle("GET /.well-known/oauth-authorization-server", s.discovery.Handler())
	}
	// JWKS: served only when the broker has a signing key. Public,
	// unauthenticated, no rate limit — same posture as the OIDC
	// discovery doc (every OAuth client library expects the JWKS
	// available without auth so it can validate id_tokens against
	// the issuer the discovery doc advertises).
	if s.jwks != nil {
		mux.Handle("GET /.well-known/jwks.json", s.jwks)
	}
	mux.Handle("GET /auth/login", s.ipRateLimit(http.HandlerFunc(s.authLogin)))
	mux.HandleFunc("GET /auth/callback", s.authCallback)
	// RFC 8628 device-flow surface. All POST surfaces sit behind the
	// IP rate limiter — they're unauthenticated by design and a
	// script can probe POST /device for the 302-vs-400 transition to
	// brute-force active user_codes; the alphabet space makes that
	// math infeasible (~30^8) but the limiter cuts the attack rate to
	// a small fixed per-IP cap as defense-in-depth. /device GET (the
	// HTML form) is unprotected: humans hand-typing codes rarely hit
	// any rate threshold, and serving a static page has no state to
	// leak.
	mux.Handle("POST /device/authorize", s.ipRateLimit(http.HandlerFunc(s.deviceAuthorize)))
	mux.HandleFunc("GET /device", s.deviceFormGet)
	mux.Handle("POST /device", s.ipRateLimit(http.HandlerFunc(s.deviceFormPost)))
	mux.Handle("POST /device/token", s.ipRateLimit(http.HandlerFunc(s.deviceToken)))
	// RFC 7591 dynamic client registration. Open (unauthenticated) per
	// the spec's §2 open-registration mode; rubber-stamps any caller
	// because the broker's actual access control lives at the
	// id_token verification + per-world Allow list, not the client_id
	// layer. IP rate limiter caps per-IP /register churn the same way
	// it gates the device-flow POSTs. See register.go for the full
	// rationale.
	mux.Handle("POST /register", s.ipRateLimit(http.HandlerFunc(s.register)))
	// /oauth/authorize stub. The discovery doc advertises this URL as
	// `authorization_endpoint` to keep MCP clients from defaulting back
	// to the upstream IdP's authorize URL (where the broker's DCR-minted
	// client_id is unknown). GET + POST both routed at the same handler
	// per RFC 6749 §3.1. See oauth_authorize.go for the probe rationale.
	mux.Handle("GET /oauth/authorize", s.ipRateLimit(http.HandlerFunc(s.oauthAuthorize)))
	mux.Handle("POST /oauth/authorize", s.ipRateLimit(http.HandlerFunc(s.oauthAuthorize)))
	// RFC 7009 token revocation. Unauthenticated (possession of
	// the token IS the authz signal) under the IP limiter for
	// defense-in-depth. Operates on refresh tokens only.
	mux.Handle("POST /token/revoke", s.ipRateLimit(http.HandlerFunc(s.tokenRevoke)))
	// /me/install verifies the bearer, rate-limits by subject, and lists
	// readable worlds without token material.
	mux.Handle("GET /me/install", s.requireAuth(s.subjectRateLimit(http.HandlerFunc(s.meInstall))))
	return mux
}

// RefreshStore exposes the broker's refresh-token store so the
// Sweeper (constructed in main.go) can share a single instance.
// Returns nil only if NewServer somehow skipped construction — not a
// production path. Kept as a method (not a public field) so the
// lifecycle remains "Server owns construction; callers borrow."
func (s *Server) RefreshStore() *RefreshStore { return s.refreshStore }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	// Slice B: readiness is just "process is up". Slice C/D wires
	// downstream-dependency probes (k8s API reach, OIDC discovery cache
	// still warm, etc.) once the surface is richer.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	nonce, err := NewNonce()
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: nonce", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := State{Nonce: nonce, ExpiresAt: s.clock().Add(s.cfg.Server.StateTTL)}
	// Device-flow handoff: if /device set the device cookie, pin the
	// device_code into the signed state HERE so /auth/callback's
	// dispatch is driven by a tamper-evident value rather than the
	// ambient cookie. The cookie itself is consumed (cleared) below
	// so an abandoned-then-resumed-elsewhere flow can't silently
	// resurrect the device branch later.
	if cookie, cerr := r.Cookie(deviceCookieName); cerr == nil && cookie.Value != "" {
		if _, ok := s.deviceStore.LookupByDeviceCode(cookie.Value); ok {
			state.DeviceCode = cookie.Value
		}
		s.clearDeviceCookie(w)
	}
	signed, err := s.signer.Sign(state)
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: sign state", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    signed,
		Path:     "/auth/callback",
		HttpOnly: true,
		Secure:   !s.cfg.Server.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  state.ExpiresAt,
		MaxAge:   int(s.cfg.Server.StateTTL.Seconds()),
	})
	http.Redirect(w, r, s.verifier.AuthCodeURL(nonce), http.StatusFound)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	queryState := r.URL.Query().Get("state")
	if queryState == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	cookie, err := r.Cookie(stateCookieName)
	if err != nil {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	state, err := s.signer.Verify(cookie.Value)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: state cookie invalid", "err", err)
		http.Error(w, "invalid state", http.StatusUnauthorized)
		return
	}
	if state.Nonce != queryState {
		s.log.WarnContext(r.Context(), "broker: state mismatch", "want", state.Nonce, "got", queryState)
		http.Error(w, "state mismatch", http.StatusUnauthorized)
		return
	}
	// State cookie has done its job — clear it before any branch so
	// even error paths can't replay the nonce.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/auth/callback",
		HttpOnly: true,
		Secure:   !s.cfg.Server.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	// Device-flow dispatch is driven by the signed State, not by an
	// ambient cookie. authLogin pins state.DeviceCode when /device set
	// the device cookie; an empty value here means a normal browser
	// code-flow callback. A stale device cookie left in the jar from
	// an abandoned flow cannot route a later browser callback through
	// the device branch because the State was minted fresh at the
	// most recent /auth/login.
	if state.DeviceCode != "" {
		s.deviceCallback(w, r, state.DeviceCode)
		return
	}
	// Auth-code dispatch (parallel to device-flow): the signed State
	// carries AuthCodeID when /oauth/authorize set the cookie. The
	// AuthCodeID was minted by authCodeStore.Begin and never left
	// the broker (it lives only in the signed cookie), so a callback
	// claiming a non-empty AuthCodeID is necessarily one we issued.
	if state.AuthCodeID != "" {
		s.authCodeCallback(w, r, state.AuthCodeID)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	exchange, err := s.verifier.Exchange(r.Context(), code)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: oauth exchange failed", "err", err)
		http.Error(w, "oauth exchange failed", http.StatusUnauthorized)
		return
	}
	claims := exchange.Claims
	if !claims.EmailVerified {
		s.log.InfoContext(r.Context(), "broker: rejected unverified identity", "subject", hashSubject(claims.Subject))
		http.Error(w, "email not verified", http.StatusForbidden)
		return
	}
	if !oidcDomainAllowed(s.cfg.OIDC.AllowDomains, claims.HD) {
		s.log.InfoContext(r.Context(), "broker: rejected by allowDomains", "subject", hashSubject(claims.Subject), "hd", claims.HD)
		http.Error(w, "domain not permitted", http.StatusForbidden)
		return
	}
	claims.Email = strings.ToLower(strings.TrimSpace(claims.Email))
	if claims.Email == "" {
		s.log.InfoContext(r.Context(), "broker: identity has no email", "subject", hashSubject(claims.Subject))
		http.Error(w, "email claim missing", http.StatusForbidden)
		return
	}
	// Publish tokens remain broker-internal; installability rules live
	// in listInstallableWorlds (one fail-closed policy site).
	out := s.listInstallableWorlds(r.Context(), &claims)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	s.log.InfoContext(r.Context(), "broker: login succeeded", "subject", hashSubject(claims.Subject), "worlds", len(out))
	writeJSON(w, http.StatusOK, installResponse{
		Email:  claims.Email,
		Worlds: out,
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	// RFC 6750 §2.1: the scheme is case-insensitive. Clients in the wild
	// send "Bearer", "bearer", and even "BEARER"; accept all of them.
	parts := strings.Fields(h)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// hashSubject returns a short, stable, non-reversible identifier suitable
// for log correlation without exposing the raw OIDC subject (which often
// embeds the user's email or external IdP ID). 8 hex chars of SHA-256 is
// enough to disambiguate users in a single broker's audit trail while
// keeping aggregated log stores PII-light. Logs only need a fingerprint,
// not the raw subject.
func hashSubject(subject string) string {
	if subject == "" {
		return ""
	}
	h := sha256.Sum256([]byte(subject))
	return "sha256-" + hex.EncodeToString(h[:4])
}
