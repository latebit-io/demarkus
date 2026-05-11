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
	cfg      *Config
	signer   *Signer
	verifier Verifier
	issuer   *Issuer
	log      *slog.Logger
	clock    func() time.Time
}

// NewServer wires a Server. The caller is responsible for constructing the
// Signer, Verifier, and Issuer in advance — keeps this constructor cheap
// enough for tests to call directly.
func NewServer(cfg *Config, signer *Signer, verifier Verifier, issuer *Issuer, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:      cfg,
		signer:   signer,
		verifier: verifier,
		issuer:   issuer,
		log:      log,
		clock:    time.Now,
	}
}

// Routes returns an http.Handler with the broker's routes registered.
// Routes are mounted on the default ServeMux pattern syntax (Go 1.22+),
// which gives us method + path matching without a router library.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /auth/login", s.authLogin)
	mux.HandleFunc("GET /auth/callback", s.authCallback)
	mux.HandleFunc("GET /tokens", s.listTokens)
	mux.HandleFunc("DELETE /tokens/{label}", s.deleteToken)
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
		Secure:   true,
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
		Secure:   true,
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
	claims, ok := s.authenticate(w, r)
	if !ok {
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
	claims, ok := s.authenticate(w, r)
	if !ok {
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

// authenticate validates the bearer ID token on the request and returns
// the verified claims. On any failure it writes the appropriate HTTP
// response and returns ok=false; the caller must return immediately.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (Claims, bool) {
	raw := bearerToken(r)
	if raw == "" {
		http.Error(w, "Authorization: Bearer <id_token> required", http.StatusUnauthorized)
		return Claims{}, false
	}
	claims, err := s.verifier.VerifyIDToken(r.Context(), raw)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: id_token verification failed", "err", err)
		http.Error(w, "invalid bearer token", http.StatusUnauthorized)
		return Claims{}, false
	}
	return claims, true
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
