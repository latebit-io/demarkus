package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Claims is the subset of OIDC ID-token claims the broker actually consumes.
// Slice B uses only Subject + Email + EmailVerified. Groups will be added
// in Slice C; left out for now so the surface area is minimal.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
}

// Verifier is the broker's abstraction over an OIDC provider. The
// production implementation wraps coreos/go-oidc + golang.org/x/oauth2;
// tests pass a fake that hard-codes claims. Slice B keeps this surface
// intentionally tiny — three calls cover both the browser code-flow
// (AuthCodeURL + Exchange) and the CLI bearer-auth path (VerifyIDToken).
type Verifier interface {
	// AuthCodeURL returns the redirect URL the user is sent to after
	// /auth/login. state is the OAuth2 state parameter (the same nonce
	// the broker stores in its signed cookie for cross-check).
	AuthCodeURL(state string) string
	// Exchange swaps a callback authorization code for verified claims.
	// It fetches the token from the IdP, verifies the ID-token signature
	// against the JWKS, and extracts the broker's needed claims.
	Exchange(ctx context.Context, code string) (Claims, error)
	// VerifyIDToken validates a bearer ID token presented by the CLI on
	// /tokens and DELETE /tokens/:label. Same signature + claims
	// extraction as Exchange's final step, without the OAuth code swap.
	VerifyIDToken(ctx context.Context, raw string) (Claims, error)
}

// ErrNoIDToken is returned by Exchange when the IdP responds without an
// id_token (would be unusual for a properly-configured OIDC client but is
// worth surfacing explicitly rather than panicking on a nil cast).
var ErrNoIDToken = errors.New("broker: oidc token response missing id_token")

// ErrEmailUnverified is returned when the IdP did not assert
// email_verified=true. The broker refuses to mint for unverified emails
// because the world authorization decision (allowDomains) is meaningless
// on an unverified address.
var ErrEmailUnverified = errors.New("broker: id_token email not verified")

type oidcVerifier struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewVerifier builds an OIDC Verifier from the broker's OIDCConfig. The
// constructor performs OIDC discovery (one HTTP call to the issuer) so it
// can fail fast at broker startup rather than on the first user login.
//
// The library is provider-agnostic: any RFC-compliant OIDC IdP with
// discovery works. "google" is just the first one we've validated.
func NewVerifier(ctx context.Context, cfg OIDCConfig) (Verifier, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("broker: oidc discovery for %s: %w", cfg.Issuer, err)
	}
	return &oidcVerifier{
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (v *oidcVerifier) AuthCodeURL(state string) string {
	return v.oauth.AuthCodeURL(state)
}

func (v *oidcVerifier) Exchange(ctx context.Context, code string) (Claims, error) {
	tok, err := v.oauth.Exchange(ctx, code)
	if err != nil {
		return Claims{}, fmt.Errorf("broker: oauth exchange: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return Claims{}, ErrNoIDToken
	}
	return v.VerifyIDToken(ctx, rawIDToken)
}

func (v *oidcVerifier) VerifyIDToken(ctx context.Context, rawIDToken string) (Claims, error) {
	idTok, err := v.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Claims{}, fmt.Errorf("broker: verify id_token: %w", err)
	}
	var raw struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idTok.Claims(&raw); err != nil {
		return Claims{}, fmt.Errorf("broker: read id_token claims: %w", err)
	}
	if !raw.EmailVerified {
		return Claims{}, ErrEmailUnverified
	}
	return Claims{
		Subject:       idTok.Subject,
		Email:         raw.Email,
		EmailVerified: raw.EmailVerified,
	}, nil
}
