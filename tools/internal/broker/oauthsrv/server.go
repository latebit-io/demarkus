package oauthsrv

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/internal/broker/core"
)

// stateCookieName is the name of the signed cookie holding the OIDC state
// nonce + expiry. The browser sends it back on /auth/callback so the
// broker can cross-check against the OAuth `state` query parameter
// (defense against login-CSRF / IdP-mixup).
const stateCookieName = "broker_oidc_state"

// Server is the broker's HTTP layer: the OAuth surface and the management
// API. Tests pass a fake Verifier and a fake clientset and exercise every
// route end-to-end without network or kube.
type Server struct {
	cfg       *core.Config
	signer    *Signer
	verifier  core.Verifier
	discovery *Discovery
	log       *slog.Logger
	clock     func() time.Time

	// deviceStore and authCodeStore hold in-flight grants in process memory;
	// a restart drops them (single broker invariant). refreshStore and
	// dynamicClients persist through the SecretStore.
	deviceStore    *deviceStore
	authCodeStore  *authCodeStore
	refreshStore   *RefreshStore
	dynamicClients *DynamicClientStore

	// idTokenSigner signs broker id_tokens on the refresh grant path and
	// serves its key at /.well-known/jwks.json; jwks is nil without it.
	idTokenSigner *core.IDTokenSigner
	jwks          *jwksHandler

	// tenantScoped mirrors the gateway profile so /me/install and
	// /auth/callback list worlds the way the gateway scopes them.
	tenantScoped bool

	// subjectReg limits /me/install per subject, loginReg /auth/login per
	// IP; nil when the limiter is disabled, and the middleware passes through.
	subjectReg *core.RateLimitRegistry
	loginReg   *core.RateLimitRegistry
	// trustForwardedFor is hoisted so ipRateLimit does not reach into Config.
	trustForwardedFor bool
}

// ServerDeps is what NewServer needs built in advance, so the constructor
// stays cheap enough for tests to call directly. Discovery and IDTokenSigner
// are optional; Routes skips their registrations when absent.
type ServerDeps struct {
	// SharedDeps carries the composed verifier, clock, log and subject
	// limiter the gateway uses too.
	core.SharedDeps
	Signer *Signer
	Store  core.SecretStore
	// Discovery requires IDTokenSigner: the document advertises jwks_uri.
	Discovery     *Discovery
	IDTokenSigner *core.IDTokenSigner
	// LoginLimiter is this listener's own, keyed by client IP.
	LoginLimiter *core.RateLimitRegistry
	// TenantScoped is the gateway profile's scoping, shared so the
	// management API's world listings can never diverge from the gateway's.
	TenantScoped bool
}

// NewServer wires a Server from its dependencies.
func NewServer(cfg *core.Config, deps ServerDeps) *Server {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	// A discovery document advertising jwks_uri at an unregistered route
	// breaks strict OIDC clients at key discovery; a programming error,
	// not a runtime state, hence the panic.
	if deps.Discovery != nil && deps.IDTokenSigner == nil {
		panic("broker: NewServer with discovery != nil requires idTokenSigner; otherwise the well-known doc would advertise jwks_uri at a route that is not registered")
	}
	// Knobs default here too so tests that build Config{} without
	// LoadConfig get the same values the validated path resolves.
	deviceTTL := cfg.Server.DeviceCodeTTL
	if deviceTTL <= 0 {
		deviceTTL = core.DefaultDeviceCodeTTL
	}
	pollInterval := cfg.Server.DevicePollInterval
	if pollInterval <= 0 {
		pollInterval = core.DefaultDevicePollInterval
	}
	if cfg.Server.RefreshTokensSecret == "" {
		cfg.Server.RefreshTokensSecret = core.DefaultRefreshTokensSecret
	}
	if cfg.Server.DynamicClientsSecret == "" {
		cfg.Server.DynamicClientsSecret = core.DefaultDynamicClientsSecret
	}
	if cfg.Server.RefreshTokenTTL <= 0 {
		cfg.Server.RefreshTokenTTL = core.DefaultRefreshTokenTTL
	}
	if cfg.Server.IDTokenTTL <= 0 {
		cfg.Server.IDTokenTTL = core.DefaultIDTokenTTL
	}
	s := &Server{
		cfg:               cfg,
		signer:            deps.Signer,
		verifier:          deps.Verifier,
		discovery:         deps.Discovery,
		idTokenSigner:     deps.IDTokenSigner,
		log:               log,
		clock:             clock,
		deviceStore:       newDeviceStore(clock, deviceTTL, pollInterval),
		authCodeStore:     newAuthCodeStore(clock, defaultPendingAuthCodeTTL, defaultAuthCodeTTL),
		refreshStore:      NewRefreshStore(cfg, deps.Store),
		dynamicClients:    NewDynamicClientStore(cfg, deps.Store),
		subjectReg:        deps.SubjectLimiter,
		loginReg:          deps.LoginLimiter,
		tenantScoped:      deps.TenantScoped,
		trustForwardedFor: cfg.RateLimit.TrustForwardedFor,
	}
	if deps.IDTokenSigner != nil {
		jwks, err := newJWKSHandler(deps.IDTokenSigner)
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
	return s
}

// Routes mounts the management API. Probes and /auth/callback run bare (the
// state cookie is the callback's gate), /auth/login behind the IP limiter,
// /me/install behind requireAuth then the shared subject limiter.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// Discovery is public and unrate-limited (fetched once per join, cached via
	// Cache-Control). The RFC 8414 path aliases the same document; §3.3 requires
	// it on the issuer's origin, which is this listener.
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
	mux.Handle("POST /device/authorize", s.ipRateLimit(limitForm(http.HandlerFunc(s.deviceAuthorize))))
	mux.HandleFunc("GET /device", s.deviceFormGet)
	mux.Handle("POST /device", s.ipRateLimit(limitForm(http.HandlerFunc(s.deviceFormPost))))
	mux.Handle("POST /device/token", s.ipRateLimit(limitForm(http.HandlerFunc(s.deviceToken))))
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
	mux.Handle("POST /oauth/authorize", s.ipRateLimit(limitForm(http.HandlerFunc(s.oauthAuthorize))))
	// RFC 7009 token revocation. Unauthenticated (possession of
	// the token IS the authz signal) under the IP limiter for
	// defense-in-depth. Operates on refresh tokens only.
	mux.Handle("POST /token/revoke", s.ipRateLimit(limitForm(http.HandlerFunc(s.tokenRevoke))))
	// /me/install verifies the bearer, rate-limits by subject, and lists
	// readable worlds without token material.
	mux.Handle("GET /me/install", s.requireAuth(core.SubjectRateLimit(s.subjectReg, s.log, http.HandlerFunc(s.meInstall))))
	return mux
}

// RefreshStore exposes the refresh token store so the Sweeper shares the
// one instance; the Server owns construction, callers borrow.
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
	if subtle.ConstantTimeCompare([]byte(state.Nonce), []byte(queryState)) != 1 {
		// Fingerprints only: the cookie holding the expected nonce is still live here.
		s.log.WarnContext(r.Context(), "broker: state mismatch", "want", core.HashSubject(state.Nonce), "got", core.HashSubject(queryState))
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
	if err := core.GateIdentity(s.cfg.OIDC.AllowDomains, &claims); err != nil {
		s.log.InfoContext(r.Context(), "broker: identity rejected", "err", err, "subject", core.HashSubject(claims.Subject), "hd", claims.HD)
		http.Error(w, strings.TrimPrefix(err.Error(), "broker: "), http.StatusForbidden)
		return
	}
	// Publish tokens remain broker-internal; installability rules live
	// in listInstallableWorlds (one fail-closed policy site).
	out := s.listInstallableWorlds(r.Context(), &claims)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	s.log.InfoContext(r.Context(), "broker: login succeeded", "subject", core.HashSubject(claims.Subject), "worlds", len(out))
	writeJSON(w, http.StatusOK, installResponse{
		Email:  claims.Email,
		Worlds: out,
	})
}

// maxFormBytes bounds an OAuth form body; the largest real field is a JWT sized token.
const maxFormBytes = 64 << 10

// limitForm caps the body before ParseForm, which otherwise reads up to 10 MB
// from an unauthenticated caller.
func limitForm(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
