package broker

import (
	"net/http"
	"net/url"
	"strings"
)

// oauthAuthorize: RFC 6749 §4.1.1 endpoint, code + PKCE-S256 only;
// pre-trust errors return JSON 400, post-trust errors redirect. Trust
// is WebClients allowlist, RFC 7591 registration, or loopback.
func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if err := r.ParseForm(); err != nil {
		writeAuthorizeJSONError(w, "invalid_request", "could not parse form")
		return
	}
	params := r.Form

	clientID := strings.TrimSpace(params.Get("client_id"))
	if clientID == "" {
		writeAuthorizeJSONError(w, "invalid_request", "client_id required")
		return
	}
	redirectURI := strings.TrimSpace(params.Get("redirect_uri"))
	if redirectURI == "" {
		writeAuthorizeJSONError(w, "invalid_request", "redirect_uri required")
		return
	}
	if webClient, ok := s.cfg.webClient(clientID); ok {
		// Registered confidential web client: the redirect target is
		// the operator-curated exact-match allowlist, not loopback.
		// RFC 6749 §3.1.2.3 — pre-registered redirect URIs are the
		// redirect-trust mechanism for confidential clients.
		if !webClient.allowsRedirect(redirectURI) {
			writeAuthorizeJSONError(w, "invalid_request", "redirect_uri is not registered for this client")
			return
		}
	} else if !isLoopbackRedirectURI(redirectURI) {
		// Non-loopback public client: trusted only when an RFC 7591
		// registration recorded this exact redirect (MCP-host path);
		// refused BEFORE trust, so no 302 to a hostile host.
		record, ok, lookupErr := s.dynamicClients.Lookup(r.Context(), clientID)
		if lookupErr != nil {
			s.log.ErrorContext(r.Context(), "broker: authorize: dynamic client lookup failed", "err", lookupErr)
			writeAuthorizeJSONError(w, "server_error", "client registry unavailable; try again")
			return
		}
		if !ok || !record.allowsRedirect(redirectURI) {
			writeAuthorizeJSONError(w, "invalid_request", "redirect_uri is not registered for this client; register it via the RFC 7591 registration_endpoint or use a loopback redirect")
			return
		}
	}

	// redirect_uri is now trusted. All subsequent errors redirect.
	clientState := params.Get("state")

	responseType := strings.TrimSpace(params.Get("response_type"))
	if responseType != "code" {
		redirectAuthorizeError(w, r, redirectURI, clientState, "unsupported_response_type",
			"only response_type=code is supported")
		return
	}
	codeChallenge := strings.TrimSpace(params.Get("code_challenge"))
	if codeChallenge == "" {
		redirectAuthorizeError(w, r, redirectURI, clientState, "invalid_request",
			"code_challenge required")
		return
	}
	codeChallengeMethod := strings.TrimSpace(params.Get("code_challenge_method"))
	if codeChallengeMethod == "" {
		// RFC 7636 §4.3: omitted method defaults to `plain`, but the
		// broker does not accept `plain`. Reject the omitted case
		// explicitly so the SDK gets a helpful error code rather
		// than the broker silently treating it as plain and then
		// failing PKCE at the /token leg.
		redirectAuthorizeError(w, r, redirectURI, clientState, "invalid_request",
			"code_challenge_method required (only S256 is supported)")
		return
	}
	if codeChallengeMethod != "S256" {
		redirectAuthorizeError(w, r, redirectURI, clientState, "invalid_request",
			"only code_challenge_method=S256 is supported")
		return
	}

	authCodeID, err := s.authCodeStore.Begin(&AuthCodeRequest{
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		ClientState:         clientState,
		Scope:               params.Get("scope"),
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
	})
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: auth code begin failed", "err", err)
		redirectAuthorizeError(w, r, redirectURI, clientState, "server_error", "internal error")
		return
	}

	nonce, err := NewNonce()
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: nonce", "err", err)
		redirectAuthorizeError(w, r, redirectURI, clientState, "server_error", "internal error")
		return
	}
	state := State{
		Nonce:      nonce,
		AuthCodeID: authCodeID,
		ExpiresAt:  s.clock().Add(s.cfg.Server.StateTTL),
	}
	signed, err := s.signer.Sign(state)
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: sign state", "err", err)
		redirectAuthorizeError(w, r, redirectURI, clientState, "server_error", "internal error")
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

// authCodeCallback resumes the auth-code flow after the IdP has
// returned the user to /auth/callback. Dispatched from authCallback
// when the verified State carries a non-empty AuthCodeID. State
// validation (signature, nonce match, expiry, cookie clear) is
// owned by the caller; this function is invoked only after those
// gates pass.
//
// On success: Exchange the IdP code, Bind into the authCodeStore
// (which mints the broker's authorization code), 302 back to the
// client's redirect_uri with `code` + `state` + `iss` (RFC 9207
// mix-up defense — cheap to ship even if Claude Code's SDK ignores
// it).
//
// On IdP-side denial (?error=...): redirect to the client's
// redirect_uri with the equivalent OAuth error code so the MCP SDK
// surfaces a real failure rather than waiting for /token.
//
// On broker-side failure (LookupPending miss, Exchange error, Bind
// error): redirect with `error=server_error` whenever the pending
// entry is still recoverable (so we have a redirect_uri to send
// the SDK to); render a generic error page only when the pending
// entry is gone entirely.
func (s *Server) authCodeCallback(w http.ResponseWriter, r *http.Request, authCodeID string) {
	pending, ok := s.authCodeStore.LookupPending(authCodeID)
	if !ok {
		// No redirect_uri to send the client to — the pending entry
		// has been swept or never existed. Surface the failure as a
		// plain 400; the MCP SDK will see it as an aborted browser
		// leg.
		s.log.WarnContext(r.Context(), "broker: auth code callback: pending entry missing",
			"authCodeID", authCodeID)
		http.Error(w, "authorization session expired", http.StatusBadRequest)
		return
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		s.log.InfoContext(r.Context(), "broker: auth code callback denied by idp", "err", errParam)
		// The IdP error code may not match the OAuth 2.0
		// client-facing set; map to access_denied as the closest
		// safe equivalent. Real failure detail stays in the log.
		redirectAuthorizeError(w, r, pending.RedirectURI, pending.ClientState,
			"access_denied", "identity provider denied the request")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.log.WarnContext(r.Context(), "broker: auth code callback missing code param")
		redirectAuthorizeError(w, r, pending.RedirectURI, pending.ClientState,
			"server_error", "missing authorization code from identity provider")
		return
	}

	exchange, err := s.verifier.Exchange(r.Context(), code)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: auth code callback oauth exchange failed", "err", err)
		// Do NOT translate to access_denied — same rationale as
		// deviceCallback. A transient IdP failure is not a user
		// denial.
		redirectAuthorizeError(w, r, pending.RedirectURI, pending.ClientState,
			"server_error", "identity provider exchange failed")
		return
	}

	if !oidcDomainAllowed(s.cfg.OIDC.AllowDomains, exchange.Claims.HD) {
		s.log.InfoContext(r.Context(), "broker: auth code rejected by allowDomains",
			"subject", hashSubject(exchange.Claims.Subject), "hd", exchange.Claims.HD)
		redirectAuthorizeError(w, r, pending.RedirectURI, pending.ClientState,
			"access_denied", "domain not permitted")
		return
	}

	authCode, _, err := s.authCodeStore.Bind(authCodeID, &exchange)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: auth code bind failed",
			"err", err, "subject", hashSubject(exchange.Claims.Subject))
		redirectAuthorizeError(w, r, pending.RedirectURI, pending.ClientState,
			"server_error", "authorization session expired")
		return
	}

	s.log.InfoContext(r.Context(), "broker: auth code bind succeeded",
		"subject", hashSubject(exchange.Claims.Subject))

	redirectAuthorizeSuccess(w, r, pending.RedirectURI, pending.ClientState, authCode, s.cfg.Server.PublicURL)
}

// writeAuthorizeJSONError emits a pre-redirect-trust error in the
// OAuth 2.0 JSON-400 shape. Only invoked while redirect_uri is not
// yet trusted (missing/malformed client_id, redirect_uri).
func writeAuthorizeJSONError(w http.ResponseWriter, code, description string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             code,
		"error_description": description,
	})
}

// redirectAuthorizeError 302s to the client's redirect_uri with
// `error=<code>&error_description=<desc>&state=<client_state>` per
// RFC 6749 §4.1.2.1. Caller MUST have validated that redirectURI
// is a loopback URI (or otherwise trusted) before invoking — this
// helper does not re-check.
func redirectAuthorizeError(w http.ResponseWriter, r *http.Request, redirectURI, clientState, code, description string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// Unreachable in practice — caller validated. Falling back
		// to a JSON 400 keeps the failure surfaced rather than
		// generating a malformed Location header.
		writeAuthorizeJSONError(w, "server_error", "invalid redirect_uri")
		return
	}
	q := u.Query()
	q.Set("error", code)
	if description != "" {
		q.Set("error_description", description)
	}
	if clientState != "" {
		q.Set("state", clientState)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// redirectAuthorizeSuccess 302s to the client's redirect_uri with
// `code=<authCode>&state=<client_state>&iss=<brokerURL>`. The `iss`
// parameter (RFC 9207) lets a defensive SDK confirm the redirect
// came from the broker it expected, defending against authorization-
// response mix-up attacks. Cheap to emit even if a given SDK ignores
// it.
func redirectAuthorizeSuccess(w http.ResponseWriter, r *http.Request, redirectURI, clientState, authCode, brokerURL string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		writeAuthorizeJSONError(w, "server_error", "invalid redirect_uri")
		return
	}
	q := u.Query()
	q.Set("code", authCode)
	if clientState != "" {
		q.Set("state", clientState)
	}
	if brokerURL != "" {
		q.Set("iss", brokerURL)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// isLoopbackRedirectURI returns true iff raw parses as
// http://127.0.0.1[:PORT][/path] or http://localhost[:PORT][/path]
// or http://[::1][:PORT][/path]. RFC 8252 §7.3 requires native apps
// use loopback; the broker refuses anything else outright.
//
// Implementation notes:
//   - Scheme is http only (TLS is irrelevant on loopback, and HTTPS
//     loopback redirects are not in the MCP SDK's repertoire).
//   - Hostname (without port) is compared as the parsed Host so an
//     attacker cannot smuggle a non-loopback target via userinfo
//     (`http://localhost@evil.example/`) — net/url's Hostname()
//     strips userinfo automatically.
//   - Port is unrestricted (RFC 8252 §7.3 explicitly permits any
//     port; native apps bind ephemeral ports).
func isLoopbackRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" {
		return false
	}
	if u.User != nil {
		// Userinfo on a redirect URI has no legitimate use here and
		// is a known SSRF / mix-up footgun. Reject outright.
		return false
	}
	host := u.Hostname()
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}
