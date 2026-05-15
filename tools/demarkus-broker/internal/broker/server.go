package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// Server is the broker's HTTP layer. It depends only on the Verifier and
// Issuer interfaces; tests pass a fakeVerifier and a fake-clientset-backed
// Issuer and exercise every route end-to-end without network or kube.
type Server struct {
	cfg       *Config
	signer    *Signer
	verifier  Verifier
	issuer    *Issuer
	discovery *Discovery
	log       *slog.Logger
	clock     func() time.Time

	// subjectReg is the per-subject limiter shared across the three
	// /tokens routes; loginReg is the per-IP limiter for /auth/login.
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

// NewServer wires a Server. The caller is responsible for constructing the
// Signer, Verifier, Issuer, and Discovery in advance — keeps this
// constructor cheap enough for tests to call directly. discovery is
// optional: tests that don't exercise the well-known route pass nil and
// Routes() skips registering it.
func NewServer(cfg *Config, signer *Signer, verifier Verifier, issuer *Issuer, discovery *Discovery, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:               cfg,
		signer:            signer,
		verifier:          verifier,
		issuer:            issuer,
		discovery:         discovery,
		log:               log,
		clock:             time.Now,
		trustForwardedFor: cfg.RateLimit.TrustForwardedFor,
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
//   - /tokens, /tokens/:label DELETE, /tokens/:label/rotate:
//     requireAuth (verifies bearer, stashes claims on ctx) → then
//     subjectRateLimit (reads claims from ctx, keys on subject hash) →
//     handler (reads claims from ctx via claimsFromCtx).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// /.well-known/openid-configuration: public, unauthenticated, no
	// rate limit. Polling-friendly: device-flow clients fetch it once
	// per /soul-join and never again; intermediaries cache via the
	// Cache-Control header the Discovery handler emits.
	if s.discovery != nil {
		mux.Handle("GET /.well-known/openid-configuration", s.discovery.Handler())
	}
	mux.Handle("GET /auth/login", s.ipRateLimit(http.HandlerFunc(s.authLogin)))
	mux.HandleFunc("GET /auth/callback", s.authCallback)
	authedSubject := func(h http.HandlerFunc) http.Handler {
		return s.requireAuth(s.subjectRateLimit(h))
	}
	mux.Handle("GET /tokens", authedSubject(s.listTokens))
	mux.Handle("DELETE /tokens/{label}", authedSubject(s.deleteToken))
	mux.Handle("POST /tokens/{label}/rotate", authedSubject(s.rotateToken))
	return mux
}

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
	code := r.URL.Query().Get("code")
	if queryState == "" || code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
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
	// Best effort: clear the cookie now that it has done its job.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/auth/callback",
		HttpOnly: true,
		Secure:   !s.cfg.Server.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	claims, err := s.verifier.Exchange(r.Context(), code)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: oauth exchange failed", "err", err)
		http.Error(w, "oauth exchange failed", http.StatusUnauthorized)
		return
	}
	results, err := s.issuer.Mint(r.Context(), claims)
	if err != nil {
		// Partial mint: some worlds succeeded before a later one failed.
		// Hand back the minted tokens so the client persists them; the
		// failed worlds are absent from both Secrets, so a re-login
		// retries them cleanly. Status 200 with a partialFailure field
		// — 207 Multi-Status would be more standards-correct but most
		// HTTP clients drop the body on non-2xx codes that aren't 200.
		if len(results) > 0 {
			s.log.WarnContext(r.Context(), "broker: partial mint", "err", err, "subject", hashSubject(claims.Subject), "minted", len(results))
			// Stable client-safe code; full err detail stays in the log
			// above so we don't leak internal Secret names or backend
			// failure modes to API consumers.
			writeJSON(w, http.StatusOK, map[string]any{
				"tokens":         results,
				"partialFailure": "one_or_more_worlds_failed",
			})
			return
		}
		if errors.Is(err, ErrNotAuthorized) {
			s.log.InfoContext(r.Context(), "broker: identity not authorized for any world", "subject", hashSubject(claims.Subject))
			http.Error(w, "no authorized worlds", http.StatusForbidden)
			return
		}
		if errors.Is(err, ErrEmailUnverified) {
			s.log.InfoContext(r.Context(), "broker: rejected unverified identity", "subject", hashSubject(claims.Subject))
			http.Error(w, "email not verified", http.StatusForbidden)
			return
		}
		s.log.ErrorContext(r.Context(), "broker: mint failed", "err", err, "subject", hashSubject(claims.Subject))
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	s.log.InfoContext(r.Context(), "broker: mint succeeded", "subject", hashSubject(claims.Subject), "worlds", len(results))
	writeJSON(w, http.StatusOK, map[string]any{"tokens": results})
}

// listTokensResponse is the public shape returned by GET /tokens. The
// caller's email comes from the verified ID token, never from a request
// parameter.
type listTokensResponse struct {
	Tokens []Issuance `json:"tokens"`
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromCtx(r.Context())
	if !ok {
		// Unreachable via Routes() composition (requireAuth always
		// runs first). The 500 makes the regression loud if a future
		// route registration forgets to chain requireAuth.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	issuances, err := s.issuer.List(r.Context(), claims.Email)
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: list issuances", "err", err, "subject", hashSubject(claims.Subject))
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, listTokensResponse{Tokens: issuances})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromCtx(r.Context())
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	label := r.PathValue("label")
	if label == "" {
		http.Error(w, "label is required", http.StatusBadRequest)
		return
	}
	err := s.issuer.Revoke(r.Context(), claims.Email, label)
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "broker: revoke succeeded", "label", label, "subject", hashSubject(claims.Subject))
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "label not found", http.StatusNotFound)
	case errors.Is(err, ErrNotOwner):
		s.log.WarnContext(r.Context(), "broker: revoke owner mismatch", "label", label, "subject", hashSubject(claims.Subject))
		http.Error(w, "not the token owner", http.StatusForbidden)
	default:
		s.log.ErrorContext(r.Context(), "broker: revoke failed", "label", label, "err", err)
		http.Error(w, "revoke failed", http.StatusInternalServerError)
	}
}

func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromCtx(r.Context())
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	label := r.PathValue("label")
	if label == "" {
		http.Error(w, "label is required", http.StatusBadRequest)
		return
	}
	minted, err := s.issuer.RotateLabel(r.Context(), claims, label)
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "broker: rotate succeeded",
			"old", label, "new", minted.Label,
			"world", minted.World, "subject", hashSubject(claims.Subject))
		writeJSON(w, http.StatusOK, minted)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "label not found", http.StatusNotFound)
	case errors.Is(err, ErrNotOwner):
		s.log.WarnContext(r.Context(), "broker: rotate owner mismatch",
			"label", label, "subject", hashSubject(claims.Subject))
		http.Error(w, "not the token owner", http.StatusForbidden)
	case errors.Is(err, ErrNotAuthorized):
		// Caller's bearer token is valid but the world's Allow
		// predicate no longer accepts them (group removed, domain
		// renamed, email taken off carve-out). Same response shape
		// as Mint's "no authorized worlds" path.
		s.log.InfoContext(r.Context(), "broker: rotate denied — caller no longer authorized for world",
			"label", label, "subject", hashSubject(claims.Subject))
		http.Error(w, "no longer authorized for this world", http.StatusForbidden)
	case minted.Label != "":
		// Soft failure: the new token was minted and recorded
		// successfully, but the old-label revoke failed (k8s API
		// blip, transient RBAC issue, etc.). The user's rotation
		// completed from their perspective — return 200 with the
		// new token; the sweeper will retire the orphan old label
		// on expiry or via drift if the operator hand-cleans.
		// Logging at WARN so operators see the soft failure without
		// it tripping HTTP error-rate alerts.
		s.log.WarnContext(r.Context(), "broker: rotate partial — old label not revoked",
			"old", label, "new", minted.Label,
			"world", minted.World, "subject", hashSubject(claims.Subject), "err", err)
		writeJSON(w, http.StatusOK, minted)
	default:
		s.log.ErrorContext(r.Context(), "broker: rotate failed", "label", label, "err", err)
		http.Error(w, "rotate failed", http.StatusInternalServerError)
	}
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
// keeping aggregated log stores PII-light. The full identity lives in the
// broker's issuances Secret; logs only need a fingerprint.
func hashSubject(subject string) string {
	if subject == "" {
		return ""
	}
	h := sha256.Sum256([]byte(subject))
	return "sha256-" + hex.EncodeToString(h[:4])
}
