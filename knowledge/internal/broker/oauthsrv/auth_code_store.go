package oauthsrv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// authCodeBytes is the code's raw entropy: 256 bits, base64url in the redirect.
const authCodeBytes = 32

// defaultAuthCodeTTL is an issued code's lifetime until /token redeems it;
// OAuth 2.1 §4.1.2 advises at most 60 seconds.
const defaultAuthCodeTTL = 60 * time.Second

// Redeem refusals; the /token handler answers both invalid_grant and logs
// the mismatch's axis, which wraps errAuthCodeMismatch.
var (
	errAuthCodeNotFound = errors.New("auth code not found")
	errAuthCodeMismatch = errors.New("auth code mismatch")
)

// AuthCodeRequest is the cleaned /oauth/authorize request. It rides the
// signed state cookie to /auth/callback, so a login crosses replicas with
// no storage; short JSON names keep the cookie small.
type AuthCodeRequest struct {
	ClientID            string `json:"c"`
	RedirectURI         string `json:"r"`
	ClientState         string `json:"s,omitempty"`
	Scope               string `json:"o,omitempty"`
	CodeChallenge       string `json:"p"`
	CodeChallengeMethod string `json:"m"`
	// Resource is the canonical RFC 8707 indicator; blank mints unbound tokens.
	Resource string `json:"u,omitempty"`
}

// authCodeStore issues and redeems authorization codes in the shared
// grant document; only a code's hash is stored, with the claims it carries.
type authCodeStore struct {
	grants  *grantStore
	codeTTL time.Duration
}

// Issue mints the code for a request the callback resolved, bound to the
// request's resource.
func (s *authCodeStore) Issue(ctx context.Context, req *AuthCodeRequest, claims *core.Claims) (string, error) {
	code, err := newAuthCode()
	if err != nil {
		return "", fmt.Errorf("auth code: %w", err)
	}
	entry := codeGrant{
		ClientID:            req.ClientID,
		RedirectURI:         req.RedirectURI,
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: req.CodeChallengeMethod,
		Claims:              claims.BoundTo(req.Resource),
	}
	err = s.grants.update(ctx, func(st *grantState) error {
		if len(st.Codes) >= maxCodeGrants {
			return errGrantStoreFull
		}
		entry.ExpiresAt = s.grants.clock().Add(s.codeTTL)
		st.Codes[hashToken(code)] = entry
		return nil
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// codeRedemption is a token request's authorization_code grant.
type codeRedemption struct {
	Code, ClientID, RedirectURI, CodeVerifier string
}

// Redeem consumes a code: client_id, redirect_uri and PKCE must match. A
// mismatch leaves the code for the rest of its 60 seconds, so a client's
// transient misconfiguration does not burn the flow; success deletes it.
func (s *authCodeStore) Redeem(ctx context.Context, r codeRedemption) (core.Claims, error) {
	var claims core.Claims
	err := s.grants.update(ctx, func(st *grantState) error {
		key := hashToken(r.Code)
		entry, ok := st.Codes[key]
		switch {
		case !ok:
			return errAuthCodeNotFound
		case subtle.ConstantTimeCompare([]byte(entry.ClientID), []byte(r.ClientID)) != 1:
			return fmt.Errorf("%w: client_id", errAuthCodeMismatch)
		case subtle.ConstantTimeCompare([]byte(entry.RedirectURI), []byte(r.RedirectURI)) != 1:
			return fmt.Errorf("%w: redirect_uri", errAuthCodeMismatch)
		case !verifyPKCE(entry.CodeChallenge, entry.CodeChallengeMethod, r.CodeVerifier):
			return fmt.Errorf("%w: PKCE verifier", errAuthCodeMismatch)
		}
		claims = entry.Claims
		delete(st.Codes, key)
		return nil
	})
	if err != nil {
		return core.Claims{}, err
	}
	return claims, nil
}

// verifyPKCE checks S256 only; /oauth/authorize refuses plain, so any other
// method here fails closed.
func verifyPKCE(challenge, method, verifier string) bool {
	if method != "S256" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(challenge), []byte(computed)) == 1
}

func newAuthCode() (string, error) {
	buf := make([]byte, authCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
