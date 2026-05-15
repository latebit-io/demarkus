package broker

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
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
// is intentionally omitted in PR3 and lands in PR4.
type deviceTokenSuccess struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
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
// URLs the user follows in a browser. The client_id form parameter is
// accepted (per RFC) but unused — the broker is the only relying
// party in this flow, so a registration check would have no security
// value. We log the supplied value at debug for observability without
// gating on it.
func (s *Server) deviceAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	if id := r.PostFormValue("client_id"); id != "" {
		s.log.DebugContext(r.Context(), "broker: device authorize", "client_id", id)
	}
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
		// Round down: a slightly-shorter advertised window is safer
		// than advertising more time than the store will honor.
		ExpiresIn: int(expiresAt.Sub(now).Seconds()),
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

// deviceToken implements POST /device/token (RFC 8628 §3.4 + §3.5).
// The polling client sends the same device_code on each call; the
// broker responds with authorization_pending until /auth/callback
// runs the OAuth exchange and Binds the result, then returns the
// success payload (raw IdP id_token + access_token) on the next poll.
func (s *Server) deviceToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	if got := r.PostFormValue("grant_type"); got != deviceGrantType {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "unsupported_grant_type"})
		return
	}
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
		writeJSON(w, http.StatusOK, deviceTokenSuccess{
			AccessToken: out.Result.AccessToken,
			IDToken:     out.Result.RawIDToken,
			TokenType:   "Bearer",
			ExpiresIn:   expiresIn,
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
// from authCallback when the device cookie is present. Validates the
// OIDC state cookie (same gates as the browser path), runs Exchange,
// and Binds the result into the deviceStore for the polling client to
// pick up on its next /device/token call. Renders device_done.html
// regardless of outcome (success / IdP-denied / Bind-failed) — the
// user-facing tab carries no actionable detail, and the polling
// client distinguishes by the next /device/token response. Logs carry
// the operational detail for the operator.
//
// IdP-denied branch translates ?error=... into deviceStore.Deny so
// the polling client sees RFC 8628 access_denied rather than timing
// out with expired_token (plan §Open Question 1 lean: cleaner UX).
func (s *Server) deviceCallback(w http.ResponseWriter, r *http.Request, deviceCode string) {
	if _, ok := s.deviceStore.LookupByDeviceCode(deviceCode); !ok {
		// Cookie pointed at a code the store doesn't know — could be
		// post-restart memory loss, post-expiry-sweep, or a forged
		// cookie. Either way nothing to recover; the polling client
		// already lost the grant. Clear the cookie on the way out so
		// the next request lands cleanly on the browser path.
		s.clearDeviceCookie(w)
		http.Error(w, "device session expired", http.StatusBadRequest)
		return
	}

	queryState := r.URL.Query().Get("state")
	if queryState == "" {
		s.clearDeviceCookie(w)
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil {
		s.clearDeviceCookie(w)
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	state, err := s.signer.Verify(stateCookie.Value)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: device callback state cookie invalid", "err", err)
		s.clearDeviceCookie(w)
		http.Error(w, "invalid state", http.StatusUnauthorized)
		return
	}
	if state.Nonce != queryState {
		s.log.WarnContext(r.Context(), "broker: device callback state mismatch", "want", state.Nonce, "got", queryState)
		s.clearDeviceCookie(w)
		http.Error(w, "state mismatch", http.StatusUnauthorized)
		return
	}
	// Both cookies have done their job. Clear now (before any
	// WriteHeader call) so a replay can't reuse them and the
	// browser does not retain them for the next request.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/auth/callback",
		HttpOnly: true,
		Secure:   !s.cfg.Server.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	s.clearDeviceCookie(w)

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
		s.log.WarnContext(r.Context(), "broker: device callback oauth exchange failed", "err", err)
		if denyErr := s.deviceStore.Deny(deviceCode); denyErr != nil {
			s.log.WarnContext(r.Context(), "broker: device deny failed", "err", denyErr)
		}
		s.renderDeviceDone(w, r)
		return
	}
	if err := s.deviceStore.Bind(deviceCode, &exchange); err != nil {
		s.log.WarnContext(r.Context(), "broker: device bind failed",
			"err", err, "subject", hashSubject(exchange.Claims.Subject))
		s.renderDeviceDone(w, r)
		return
	}
	s.log.InfoContext(r.Context(), "broker: device bind succeeded",
		"subject", hashSubject(exchange.Claims.Subject))
	s.renderDeviceDone(w, r)
}

// RunDeviceJanitor sweeps the deviceStore on a fixed cadence until ctx
// is canceled. Sweep cadence is half the configured device-code TTL so
// terminal entries are evicted within one TTL window after resolution.
// Unlike the Sweeper, this janitor needs no leader election: the store
// is per-replica in-memory state (plan §"Single broker only for now"),
// so each replica sweeps its own store. Multi-replica deployments are
// out of scope until the device store grows a shared backing store.
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
		}
	}
}

// sameOriginPost returns false when the request carries an Origin
// header whose host differs from r.Host — the canonical signal of a
// cross-origin browser POST. Absence of Origin is treated as
// "not a browser" (curl, Go's http.Client, server-to-server tooling
// — none of which are CSRF-vulnerable, since CSRF requires an
// authenticated user-agent the attacker can puppet).
//
// We don't compare against cfg.Server.PublicURL because in dev /
// kind / port-forward setups the user reaches the broker through a
// host that differs from PublicURL; r.Host reflects the actual host
// the browser sees and the Origin it sends, which is what we want
// for a strict same-origin gate.
func sameOriginPost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host
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
