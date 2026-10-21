package oauthsrv

import (
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// ConsentPending is an authorize request the IdP has verified a user for,
// waiting on that user's approval of the client; it rides the consent
// cookie, so nothing is stored.
type ConsentPending struct {
	Request *AuthCodeRequest `json:"r"`
	Claims  core.Claims      `json:"i"`
}

const (
	consentCookieName = "broker_consent"
	consentPath       = "/oauth/consent"
)

//go:embed templates/consent.html
var consentTemplateFS embed.FS

var consentTemplate = template.Must(template.ParseFS(consentTemplateFS, "templates/consent.html"))

type consentData struct {
	ClientName   string
	ClientID     string
	RedirectHost string
	Resource     string
	Email        string
	Nonce        string
}

// askConsent holds the verified request in a signed cookie and shows the
// user who asked; the code is issued only by the form's approval.
func (s *Server) askConsent(w http.ResponseWriter, r *http.Request, pending *AuthCodeRequest, claims *core.Claims) {
	reply := &authorizeReply{w: w, r: r, redirectURI: pending.RedirectURI, clientState: pending.ClientState}
	nonce, err := s.setStateCookie(w, consentCookieName, consentPath, State{Consent: &ConsentPending{Request: pending, Claims: *claims}})
	if errors.Is(err, errStateTooLarge) {
		reply.fail("invalid_request", "authorization request too large")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "broker: consent state", "err", err)
		reply.fail("server_error", "internal error")
		return
	}
	data := consentData{ClientName: pending.ClientName, ClientID: pending.ClientID, Resource: pending.Resource, Email: claims.Email, Nonce: nonce}
	if data.ClientName == "" {
		data.ClientName = pending.ClientID
	}
	if u, err := url.Parse(pending.RedirectURI); err == nil {
		data.RedirectHost = u.Host
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := consentTemplate.ExecuteTemplate(w, "consent.html", data); err != nil {
		// The status is in flight; a broken template is a build fault.
		s.log.ErrorContext(r.Context(), "broker: consent render", "err", err)
	}
}

// oauthConsent takes the user's decision: approve issues the code, anything
// else refuses it. The form nonce must match the cookie's, so a page the
// user never saw cannot approve for them.
func (s *Server) oauthConsent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOriginPost(r) {
		s.log.WarnContext(r.Context(), "broker: consent POST blocked: cross-origin", "origin", r.Header.Get("Origin"), "host", r.Host)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse form", http.StatusBadRequest)
		return
	}
	cookie, err := r.Cookie(consentCookieName)
	if err != nil {
		http.Error(w, "missing consent cookie", http.StatusBadRequest)
		return
	}
	state, err := s.signer.Verify(cookie.Value)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: consent cookie invalid", "err", err)
		http.Error(w, "invalid consent", http.StatusUnauthorized)
		return
	}
	// One decision per cookie, whichever way it goes.
	s.clearCookie(w, consentCookieName, consentPath)
	if state.Consent == nil || state.Consent.Request == nil ||
		subtle.ConstantTimeCompare([]byte(state.Nonce), []byte(r.Form.Get("nonce"))) != 1 {
		http.Error(w, "consent mismatch", http.StatusUnauthorized)
		return
	}
	pending, claims := state.Consent.Request, state.Consent.Claims
	reply := &authorizeReply{w: w, r: r, redirectURI: pending.RedirectURI, clientState: pending.ClientState}
	if r.Form.Get("decision") != "approve" {
		s.log.InfoContext(r.Context(), "broker: consent declined", "subject", core.HashSubject(claims.Subject))
		reply.fail("access_denied", "the user declined the request")
		return
	}
	authCode, err := s.authCodeStore.Issue(r.Context(), pending, &claims)
	if err != nil {
		s.log.WarnContext(r.Context(), "broker: auth code bind failed", "err", err, "subject", core.HashSubject(claims.Subject))
		reply.fail(grantError(err))
		return
	}
	s.log.InfoContext(r.Context(), "broker: auth code bind succeeded after consent", "subject", core.HashSubject(claims.Subject))
	reply.succeed(authCode, s.cfg.Server.PublicURL)
}
