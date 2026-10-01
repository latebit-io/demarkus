package oauthsrv

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
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
	if webClient, ok := s.cfg.WebClient(clientID); ok {
		// Registered confidential web client: the redirect target is
		// the operator-curated exact-match allowlist, not loopback.
		// RFC 6749 §3.1.2.3 — pre-registered redirect URIs are the
		// redirect-trust mechanism for confidential clients.
		if !webClient.AllowsRedirect(redirectURI) {
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
	reply := &authorizeReply{w: w, r: r, redirectURI: redirectURI, clientState: clientState}

	responseType := strings.TrimSpace(params.Get("response_type"))
	if responseType != "code" {
		reply.fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	codeChallenge := strings.TrimSpace(params.Get("code_challenge"))
	if codeChallenge == "" {
		reply.fail("invalid_request", "code_challenge required")
		return
	}
	codeChallengeMethod := strings.TrimSpace(params.Get("code_challenge_method"))
	if codeChallengeMethod == "" {
		// RFC 7636 §4.3: omitted method defaults to `plain`, but the
		// broker does not accept `plain`. Reject the omitted case
		// explicitly so the SDK gets a helpful error code rather
		// than the broker silently treating it as plain and then
		// failing PKCE at the /token leg.
		reply.fail("invalid_request", "code_challenge_method required (only S256 is supported)")
		return
	}
	if codeChallengeMethod != "S256" {
		reply.fail("invalid_request", "only code_challenge_method=S256 is supported")
		return
	}
	resource, err := s.resourceParam(params.Get("resource"))
	if err != nil {
		// RFC 8707 §2: an unknown resource is invalid_target.
		reply.fail("invalid_target", err.Error())
		return
	}

	nonce, err := s.setAuthCodeStateCookie(w, &AuthCodeRequest{
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		ClientState:         clientState,
		Scope:               params.Get("scope"),
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		Resource:            resource,
	})
	if errors.Is(err, errStateTooLarge) {
		reply.fail("invalid_request", "authorization request too large")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: auth code state", "err", err)
		reply.fail("server_error", "internal error")
		return
	}
	http.Redirect(w, r, s.verifier.AuthCodeURL(nonce), http.StatusFound)
}

// maxStateCookieBytes keeps the signed state under the 4 KiB browsers allow
// a cookie, with room for its name and attributes.
const maxStateCookieBytes = 3500

// errStateTooLarge refuses a request whose state, scope or redirect would
// not fit the cookie that carries it to the callback.
var errStateTooLarge = errors.New("signed state exceeds the cookie budget")

// setAuthCodeStateCookie signs the authorize request into the callback
// cookie and returns its nonce, the state parameter sent to the IdP.
func (s *Server) setAuthCodeStateCookie(w http.ResponseWriter, req *AuthCodeRequest) (string, error) {
	nonce, err := NewNonce()
	if err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	state := State{
		Nonce:     nonce,
		AuthCode:  req,
		ExpiresAt: s.clock().Add(s.cfg.Server.StateTTL),
	}
	signed, err := s.signer.Sign(state)
	if err != nil {
		return "", fmt.Errorf("sign state: %w", err)
	}
	if len(signed) > maxStateCookieBytes {
		return "", errStateTooLarge
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
	return nonce, nil
}

// authCodeCallback resumes the request the signed state carried: exchange
// the IdP code, gate the identity, issue the broker's code and redirect with
// code, state and RFC 9207 iss, or with the matching OAuth error.
func (s *Server) authCodeCallback(w http.ResponseWriter, r *http.Request, pending *AuthCodeRequest) {
	reply := &authorizeReply{w: w, r: r, redirectURI: pending.RedirectURI, clientState: pending.ClientState}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		s.log.InfoContext(r.Context(), "broker: auth code callback denied by idp", "err", errParam)
		// The IdP error code may not match the OAuth 2.0
		// client-facing set; map to access_denied as the closest
		// safe equivalent. Real failure detail stays in the log.
		reply.fail("access_denied", "identity provider denied the request")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.log.WarnContext(r.Context(), "broker: auth code callback missing code param")
		reply.fail("server_error", "missing authorization code from identity provider")
		return
	}

	exchange, err := s.verifier.Exchange(r.Context(), code)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: auth code callback oauth exchange failed", "err", err)
		// Do NOT translate to access_denied — same rationale as
		// deviceCallback. A transient IdP failure is not a user
		// denial.
		reply.fail("server_error", "identity provider exchange failed")
		return
	}

	if err := core.GateIdentity(s.cfg.OIDC.AllowDomains, &exchange.Claims); err != nil {
		s.log.InfoContext(r.Context(), "broker: auth code identity rejected", "err", err,
			"subject", core.HashSubject(exchange.Claims.Subject), "hd", exchange.Claims.HD)
		reply.fail("access_denied", strings.TrimPrefix(err.Error(), "broker: "))
		return
	}

	authCode, err := s.authCodeStore.Issue(r.Context(), pending, &exchange.Claims)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: auth code bind failed",
			"err", err, "subject", core.HashSubject(exchange.Claims.Subject))
		reply.fail(grantError(err))
		return
	}

	s.log.InfoContext(r.Context(), "broker: auth code bind succeeded",
		"subject", core.HashSubject(exchange.Claims.Subject))

	reply.succeed(authCode, s.cfg.Server.PublicURL)
}

// grantError maps a grant store failure to its RFC 6749 error code; a full
// store is the client's cue to retry later, anything else is ours.
func grantError(err error) (code, description string) {
	if errors.Is(err, errGrantStoreFull) {
		return "temporarily_unavailable", "too many pending grants"
	}
	return "server_error", "internal error"
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

// authorizeReply answers an authorize leg at the client's redirect_uri,
// echoing its state. The caller has validated the URI as loopback or
// registered; this type does not re-check.
type authorizeReply struct {
	w           http.ResponseWriter
	r           *http.Request
	redirectURI string
	clientState string
}

// fail 302s with error and error_description per RFC 6749 4.1.2.1.
func (a *authorizeReply) fail(code, description string) {
	a.redirect(func(q url.Values) {
		q.Set("error", code)
		if description != "" {
			q.Set("error_description", description)
		}
	})
}

// succeed 302s with the code and, per RFC 9207, iss, so a defensive SDK
// can tell the redirect came from the broker it expected.
func (a *authorizeReply) succeed(authCode, brokerURL string) {
	a.redirect(func(q url.Values) {
		q.Set("code", authCode)
		if brokerURL != "" {
			q.Set("iss", brokerURL)
		}
	})
}

func (a *authorizeReply) redirect(set func(url.Values)) {
	u, err := url.Parse(a.redirectURI)
	if err != nil {
		// The caller validated the URI; a JSON 400 still beats a malformed Location.
		writeAuthorizeJSONError(a.w, "server_error", "invalid redirect_uri")
		return
	}
	q := u.Query()
	set(q)
	if a.clientState != "" {
		q.Set("state", a.clientState)
	}
	u.RawQuery = q.Encode()
	http.Redirect(a.w, a.r, u.String(), http.StatusFound)
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
