package broker

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// deviceCookieName is the cookie carrying the in-flight device_code from
// the /device form submit through to /auth/callback. Separate from
// stateCookieName (broker_oidc_state) because the two have different
// paths and lifetimes: the state cookie scopes to /auth/callback and
// expires with StateTTL; the device cookie scopes to / so the form
// handler can set it and the callback can read it, and lives as long as
// the device-code TTL.
const deviceCookieName = "broker_device_code"

// deviceGrantType is the RFC 8628 §3.4 grant_type identifier the polling
// client must send to /device/token. Reproduced as a constant so a typo
// in the handler can't silently accept arbitrary grant types.
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// refreshGrantType is the RFC 6749 §6 grant_type identifier the
// /device/token handler dispatches on for refresh requests.
// Reproduced as a constant for the same reason as deviceGrantType:
// the dispatch must reject typos rather than silently fall through.
const refreshGrantType = "refresh_token"

// authCodeGrantType is the RFC 6749 §4.1.3 grant_type identifier the
// /device/token handler dispatches on for the authorization-code
// exchange. Same single-source-of-truth rationale as the other two.
const authCodeGrantType = "authorization_code"

//go:embed templates/device_form.html templates/device_done.html
var deviceTemplatesFS embed.FS

// deviceTemplates is the parsed template set. Parsed eagerly at process
// start (via init) so a malformed template fails the binary rather than
// the first user request. Both pages are self-contained; the embed.FS
// keeps them inside the binary so the broker image needs no external
// assets to render the device-flow surface.
var deviceTemplates = template.Must(template.ParseFS(deviceTemplatesFS,
	"templates/device_form.html",
	"templates/device_done.html"))

// deviceFormData drives the user_code entry page. PrefilledUserCode is
// non-empty when the user followed verification_uri_complete (the
// authorize response's helper URL that embeds the code as a query
// param); Error is set when a previous submit failed and we re-render
// with feedback.
type deviceFormData struct {
	PrefilledUserCode string
	Error             string
}

// deviceAuthorizeResponse mirrors RFC 8628 §3.2. Field names are JSON-
// spec-defined; do not rename.
type deviceAuthorizeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceTokenSuccess mirrors RFC 8628 §3.5 success — the same shape as
// an OAuth2 token endpoint response when the polling completes. We
// forward the IdP's raw id_token + access_token verbatim; refresh_token
// lands at PR4 (broker-minted opaque token; see refresh.go). On a
// successful device-code completion every response carries all four
// token fields — omitempty stays on RefreshToken purely as belt-and-
// suspenders against a degenerate Bind path that didn't mint one.
type deviceTokenSuccess struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// deviceTokenError is the standard OAuth2 error response shape (RFC
// 6749 §5.2 + RFC 8628 §3.5 augmentation). Codes used here:
// authorization_pending, slow_down, expired_token, access_denied,
// invalid_grant, invalid_request, unsupported_grant_type.
type deviceTokenError struct {
	Error string `json:"error"`
}

// deviceAuthorize implements POST /device/authorize (RFC 8628 §3.1).
// Generates a fresh device_code + user_code, stashes a pending state
// in the deviceStore, and returns the codes plus the verification
// URLs the user follows in a browser.
//
// client_id is REQUIRED per RFC 8628 §3.1; missing or empty returns
// invalid_request. The broker does not validate the value against a
// registration table — the plugin clients (demarkus-join + Claude
// Code plugin) are not multiplexed identities and a registration
// check would have no security value here. The check exists to
// filter malformed clients and to keep the protocol surface
// spec-conformant for any standard OIDC device-flow library a user
// might plug in. Value is logged at debug for observability only.
func (s *Server) deviceAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	clientID := r.PostFormValue("client_id")
	if clientID == "" {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	s.log.DebugContext(r.Context(), "broker: device authorize", "client_id", clientID)
	deviceCode, userCode, expiresAt, err := s.deviceStore.Authorize()
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: device authorize failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.clock()
	verificationURI := s.cfg.Server.PublicURL + "/device"
	verificationURIComplete := verificationURI + "?user_code=" + url.QueryEscape(userCode)
	writeJSON(w, http.StatusOK, deviceAuthorizeResponse{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: verificationURIComplete,
		// Round down + clamp to 0 under any clock skew: advertising
		// a negative window confuses clients, and a slightly-shorter
		// window is safer than advertising more time than the store
		// will honor.
		ExpiresIn: max(int(expiresAt.Sub(now).Seconds()), 0),
		Interval:  int(s.cfg.Server.DevicePollInterval.Seconds()),
	})
}

// deviceFormGet renders the user_code entry page. If verification_uri_
// complete was followed the user_code query param is prefilled into
// the form so the user only has to click submit, but they can still
// edit it before submitting (matches the behavior most IdP device
// flows ship with).
func (s *Server) deviceFormGet(w http.ResponseWriter, r *http.Request) {
	data := deviceFormData{PrefilledUserCode: r.URL.Query().Get("user_code")}
	s.renderDeviceForm(w, r, data, http.StatusOK)
}

// deviceFormPost handles the user_code submit. Validates the code
// resolves to a pending grant, sets the device cookie, and 302s to
// /auth/login to start the OIDC dance. On invalid codes the form
// re-renders with an error message — distinguished only as "invalid or
// expired" since exposing whether a specific code was issued would
// turn this endpoint into an enumeration oracle.
//
// CSRF defense: a cross-origin POST that successfully reached this
// handler could bind a victim's IdP identity to an attacker-supplied
// device_code (the attacker would have called /device/authorize first
// and embedded their own user_code in a form on a page the victim
// visits). SameSite=Lax doesn't help here because we are SETTING the
// device cookie cross-site, not reading an existing one. The Origin
// header check below catches every browser-shape CSRF attempt: modern
// browsers always send Origin on POST, so any cross-origin POST will
// have an Origin pointing at the attacker's site, which we reject.
// Non-browser clients (curl, Go's http.Client) omit Origin and are
// allowed; they are not vulnerable to the CSRF tricked-browser
// scenario by construction.
func (s *Server) deviceFormPost(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		s.log.WarnContext(r.Context(), "broker: device form POST blocked — cross-origin",
			"origin", r.Header.Get("Origin"), "host", r.Host)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderDeviceForm(w, r, deviceFormData{Error: "invalid request"}, http.StatusBadRequest)
		return
	}
	userCode := r.PostFormValue("user_code")
	deviceCode, ok := s.deviceStore.LookupByUserCode(userCode)
	if !ok {
		s.renderDeviceForm(w, r, deviceFormData{
			PrefilledUserCode: userCode,
			Error:             "Invalid or expired code. Restart the join command and try again.",
		}, http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookieName,
		Value:    deviceCode,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.Server.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.Server.DeviceCodeTTL.Seconds()),
	})
	http.Redirect(w, r, "/auth/login", http.StatusFound)
}

// deviceToken implements POST /device/token. RFC 8628 §3.4+§3.5
// (device-code polling) and RFC 6749 §6 (refresh grant) share the
// same endpoint URL but are dispatched on the grant_type form
// parameter; the strict switch rejects anything else so a typo on
// the client side cannot accidentally exercise either branch's
// surface.
func (s *Server) deviceToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	switch r.PostFormValue("grant_type") {
	case deviceGrantType:
		s.deviceTokenDeviceFlow(w, r)
	case refreshGrantType:
		s.deviceTokenRefresh(w, r)
	case authCodeGrantType:
		s.deviceTokenAuthCode(w, r)
	default:
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "unsupported_grant_type"})
	}
}

// deviceTokenDeviceFlow handles the RFC 8628 §3.4 device-code poll.
// The polling client sends the same device_code on each call; the
// broker responds with authorization_pending until /auth/callback
// runs the OAuth exchange and Binds the result, then returns the
// success payload (raw IdP id_token + access_token + broker-minted
// refresh_token) on the next poll.
func (s *Server) deviceTokenDeviceFlow(w http.ResponseWriter, r *http.Request) {
	deviceCode := r.PostFormValue("device_code")
	if deviceCode == "" {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	out := s.deviceStore.Poll(deviceCode)
	switch {
	case out.Status == statusComplete:
		now := s.clock()
		expiresIn := 0
		if !out.Result.Expiry.IsZero() {
			expiresIn = max(int(out.Result.Expiry.Sub(now).Seconds()), 0)
		}
		// Tokens are bearer credentials — any intermediary that
		// caches this response is a credential-leak vector. Set the
		// canonical OAuth2 §5.1 no-store headers BEFORE writeJSON
		// fires WriteHeader so they actually land in the response.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusOK, deviceTokenSuccess{
			AccessToken:  out.Result.AccessToken,
			IDToken:      out.Result.RawIDToken,
			RefreshToken: out.RefreshToken,
			TokenType:    "Bearer",
			ExpiresIn:    expiresIn,
		})
	case out.Status == statusExpired:
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "expired_token"})
	case out.Status == statusDenied:
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "access_denied"})
	case out.SlowDown:
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "slow_down"})
	default:
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "authorization_pending"})
	}
}

// deviceTokenRefresh handles RFC 6749 §6 refresh_token exchange.
// Validates the supplied refresh_token against the broker's
// Secret-backed refreshStore, mints a fresh broker-signed id_token
// from the cached Claims, and returns the response with the SAME
// refresh_token (PR4 does not rotate — see plan §Out of Scope).
//
// access_token == id_token: PR5's /me/install consumes the id_token
// as a bearer, the broker's compositeVerifier accepts it on either
// field, and an empty access_token would force clients to learn a
// per-broker quirk. Same string in both fields keeps the
// OIDC-client-library default codepath working.
//
// Error mapping:
//   - Missing refresh_token form param → invalid_request
//   - refreshStore rejects (unknown / expired / revoked) →
//     invalid_grant per RFC 6749 §5.2
//   - Signing failure (programming error) → 500 server_error
//   - id-token-signer not wired (operator misconfig) → 500
//     server_error; PR4 wiring requires it always
func (s *Server) deviceTokenRefresh(w http.ResponseWriter, r *http.Request) {
	if s.idTokenSigner == nil {
		// Broker started without a signing key — refresh grant
		// cannot mint a verifiable id_token. Surface 500 rather
		// than silently returning an empty token; LoadConfig
		// validation in Step 6 makes this unreachable in
		// production, but keep the guard so a test misconfig
		// fails loudly.
		s.log.ErrorContext(r.Context(), "broker: refresh grant requested but id_token signer not wired")
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}
	rawRefresh := r.PostFormValue("refresh_token")
	if rawRefresh == "" {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	record, err := s.refreshStore.Refresh(r.Context(), rawRefresh)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenInvalid) {
			writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_grant"})
			return
		}
		s.log.ErrorContext(r.Context(), "broker: refresh store read failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}
	now := s.clock()
	idToken, err := s.idTokenSigner.Sign(record.Claims, s.cfg.Server.PublicURL, s.cfg.Server.IDTokenTTL, now)
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: refresh sign id_token failed",
			"err", err, "subject", hashSubject(record.Claims.Subject))
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}
	expiresIn := max(int(s.cfg.Server.IDTokenTTL.Seconds()), 0)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, deviceTokenSuccess{
		AccessToken:  idToken,
		IDToken:      idToken,
		RefreshToken: rawRefresh,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
	})
}

// deviceTokenAuthCode handles RFC 6749 §4.1.3 authorization-code
// exchange. Dispatched from deviceToken on grant_type=
// authorization_code. Validates the supplied code against the
// authCodeStore (existence, expiry, client_id binding, redirect_uri
// binding, PKCE S256 verifier), mints a fresh broker refresh_token
// + broker-signed id_token, and returns the standard OAuth2 token
// response.
//
// access_token == id_token: same convention deviceTokenRefresh
// uses (see its doc above) — the broker is the relying party for
// its own bearer tokens, the compositeVerifier accepts either
// field, and empty access_token would force every client to learn
// a per-broker quirk.
//
// Error mapping:
//   - Missing required form params → invalid_request
//   - authCodeStore.Redeem error (unknown code, mismatch, PKCE) →
//     invalid_grant. We deliberately collapse the store's distinct
//     sentinels into one wire code so the response surface does
//     not leak which axis failed (operator logs name the failure
//     for in-the-room diagnosis).
//   - Signing key not wired (operator misconfig) → server_error
//   - Refresh-token issuance failure → server_error
//   - id_token signing failure (programming error) → server_error
//
// The auth code is consumed by Redeem only on validation success;
// transient client errors (wrong client_id, wrong redirect_uri,
// wrong code_verifier) preserve the code for legitimate retry
// inside the 60-second window. See authCodeStore.Redeem's
// "Consumption semantics" note for the contract.
func (s *Server) deviceTokenAuthCode(w http.ResponseWriter, r *http.Request) {
	if s.idTokenSigner == nil {
		s.log.ErrorContext(r.Context(), "broker: auth_code grant requested but id_token signer not wired")
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}
	code := r.PostFormValue("code")
	clientID := r.PostFormValue("client_id")
	redirectURI := r.PostFormValue("redirect_uri")
	codeVerifier := r.PostFormValue("code_verifier")
	if code == "" || clientID == "" || redirectURI == "" || codeVerifier == "" {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}

	exchange, err := s.authCodeStore.Redeem(code, clientID, redirectURI, codeVerifier)
	if err != nil {
		// Log the specific axis at debug so an operator can tell
		// apart wrong-verifier (client bug) from unknown-code
		// (replay or storage churn); the wire response stays
		// invalid_grant uniformly.
		s.log.DebugContext(r.Context(), "broker: auth code redeem failed", "err", err)
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_grant"})
		return
	}

	rawRefresh, err := s.refreshStore.Issue(r.Context(), exchange.Claims, s.cfg.Server.RefreshTokenTTL)
	if err != nil {
		// Code is already consumed by Redeem (one-shot). The user
		// has to retry from /oauth/authorize; that's acceptable
		// because the failure mode is a backend write to Secrets,
		// rare in practice and resolved without user action by the
		// time they retry.
		s.log.ErrorContext(r.Context(), "broker: auth code refresh mint failed",
			"err", err, "subject", hashSubject(exchange.Claims.Subject))
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}

	now := s.clock()
	idToken, err := s.idTokenSigner.Sign(exchange.Claims, s.cfg.Server.PublicURL, s.cfg.Server.IDTokenTTL, now)
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: auth code sign id_token failed",
			"err", err, "subject", hashSubject(exchange.Claims.Subject))
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}
	expiresIn := max(int(s.cfg.Server.IDTokenTTL.Seconds()), 0)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, deviceTokenSuccess{
		AccessToken:  idToken,
		IDToken:      idToken,
		RefreshToken: rawRefresh,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
	})
}

// renderDeviceForm writes the user_code entry HTML. Templates are
// pre-parsed (deviceTemplates) so a render failure is a programming
// error rather than a runtime configuration concern.
func (s *Server) renderDeviceForm(w http.ResponseWriter, r *http.Request, data deviceFormData, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := deviceTemplates.ExecuteTemplate(w, "device_form.html", data); err != nil {
		// Logged-only: the response status + body is already in flight
		// (WriteHeader fires before ExecuteTemplate writes any markup),
		// so http.Error here would corrupt the response. The render
		// failure means there's broken template state in the binary and
		// the only sane recovery is a broker rebuild.
		s.log.ErrorContext(r.Context(), "broker: device form render", "err", err)
	}
}

// renderDeviceDone writes the "you can close this tab" confirmation
// after a successful device-flow callback. Same render-failure posture
// as renderDeviceForm.
func (s *Server) renderDeviceDone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := deviceTemplates.ExecuteTemplate(w, "device_done.html", nil); err != nil {
		s.log.ErrorContext(r.Context(), "broker: device done render", "err", err)
	}
}

// deviceCallback is /auth/callback's device-flow branch — dispatched
// by authCallback when the verified State carries a non-empty
// DeviceCode. State validation (signature, nonce match, expiry,
// cookie clear) is owned by the caller; this function is invoked
// only after those gates pass.
//
// Runs Exchange and Binds the result into the deviceStore for the
// polling client to pick up. Renders device_done.html regardless of
// outcome (success / IdP-denied / Bind-failed) — the user-facing tab
// carries no actionable detail, and the polling client distinguishes
// by the next /device/token response. Logs carry the operational
// detail for the operator.
//
// IdP-denied branch translates ?error=... into deviceStore.Deny so
// the polling client sees RFC 8628 access_denied rather than timing
// out with expired_token (plan §Open Question 1 lean: cleaner UX).
func (s *Server) deviceCallback(w http.ResponseWriter, r *http.Request, deviceCode string) {
	if _, ok := s.deviceStore.LookupByDeviceCode(deviceCode); !ok {
		// State carried a device_code the store doesn't recognize —
		// could be post-restart memory loss, post-expiry sweep, or
		// the grant was already resolved. The polling client (if any)
		// already lost the grant.
		http.Error(w, "device session expired", http.StatusBadRequest)
		return
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		s.log.InfoContext(r.Context(), "broker: device callback denied by idp", "err", errParam)
		if denyErr := s.deviceStore.Deny(deviceCode); denyErr != nil {
			s.log.WarnContext(r.Context(), "broker: device deny failed", "err", denyErr)
		}
		s.renderDeviceDone(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	exchange, err := s.verifier.Exchange(r.Context(), code)
	if err != nil {
		// Do NOT translate broker-side exchange failures into
		// access_denied: that error is reserved for the user
		// explicitly denying at the IdP (handled above on the
		// `error` query param). Calling Deny here would permanently
		// tell the polling client "user rejected" on a transient
		// failure (JWKS unreachable, IdP rate-limited, etc.). Leave
		// the grant pending so the user can retry by re-running
		// /soul-join; if the broker stays broken until the device
		// code's TTL, the polling client sees expired_token, which
		// is the truthful state.
		s.log.WarnContext(r.Context(), "broker: device callback oauth exchange failed", "err", err)
		s.renderDeviceDone(w, r)
		return
	}
	// Mint the refresh token BEFORE Bind so a Secret-side failure
	// keeps the grant pending — the polling client retries on the
	// next interval, the user re-runs soul-join, and no orphan
	// statusComplete state goes out without its refresh token.
	// Orphaned refresh-tokens Secret entries (mint succeeded, Bind
	// then races a sweep) age out on Sweep via their expiry.
	rawRefresh, err := s.refreshStore.Issue(r.Context(), exchange.Claims, s.cfg.Server.RefreshTokenTTL)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: device callback refresh mint failed",
			"err", err, "subject", hashSubject(exchange.Claims.Subject))
		s.renderDeviceDone(w, r)
		return
	}
	if err := s.deviceStore.Bind(deviceCode, &exchange, rawRefresh); err != nil {
		s.log.WarnContext(r.Context(), "broker: device bind failed",
			"err", err, "subject", hashSubject(exchange.Claims.Subject))
		s.renderDeviceDone(w, r)
		return
	}
	s.log.InfoContext(r.Context(), "broker: device bind succeeded",
		"subject", hashSubject(exchange.Claims.Subject))
	s.renderDeviceDone(w, r)
}

// RunDeviceJanitor sweeps the deviceStore and the authCodeStore on a
// fixed cadence until ctx is canceled. Sweep cadence is half the
// configured device-code TTL so terminal device-flow entries are
// evicted within one TTL window after resolution; the same tick
// also sweeps the auth-code store, whose own TTLs (10m pending,
// 60s code) are correctly enforced lazily by LookupPending/Redeem
// regardless of sweep timing — the sweep is only a memory-cleanup
// concern for abandoned grants.
//
// Unlike the Sweeper, this janitor needs no leader election: the
// stores are per-replica in-memory state (plan §"Single broker only
// for now"), so each replica sweeps its own. Multi-replica
// deployments are out of scope until either store grows a shared
// backing store.
func (s *Server) RunDeviceJanitor(ctx context.Context) {
	ttl := s.cfg.Server.DeviceCodeTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	tick := max(ttl/2, time.Second)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.deviceStore.Sweep()
			if s.authCodeStore != nil {
				s.authCodeStore.Sweep()
			}
		}
	}
}

// sameOriginPost returns false when the request carries an Origin
// header whose scheme+host differs from the request's own — the
// canonical signal of a cross-origin browser POST. Both axes
// matter: `http://broker.example.com` and `https://broker.example.com`
// are distinct origins, so a host-only match would let an attacker on
// a downgraded scheme through.
//
// Absence of Origin is treated as "not a browser" (curl, Go's
// http.Client, server-to-server tooling — none of which are CSRF-
// vulnerable, since CSRF requires an authenticated user-agent the
// attacker can puppet).
//
// Expected scheme: X-Forwarded-Proto wins when present (we are
// behind a TLS-terminating ingress per the standard deployment
// shape; a browser-driven CSRF cannot forge this header since it is
// stripped/rewritten by the ingress), falling back to r.TLS to
// distinguish direct HTTPS from plain HTTP. Expected host: r.Host
// — what the browser actually saw — rather than cfg.Server.PublicURL,
// so dev / kind / port-forward setups where users reach the broker
// on a different host than PublicURL keep working.
func sameOriginPost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return false
	}
	expectedScheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		expectedScheme = strings.ToLower(proto)
	} else if r.TLS != nil {
		expectedScheme = "https"
	}
	return u.Host == r.Host && strings.EqualFold(u.Scheme, expectedScheme)
}

// clearDeviceCookie zeroes the device cookie. Called from /auth/callback's
// device branch after Bind (or Deny) so the cookie can't replay against a
// later flow. Mirrors the state-cookie clearing pattern in authCallback.
func (s *Server) clearDeviceCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.Server.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
