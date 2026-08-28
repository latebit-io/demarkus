package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Ensure compositeVerifier satisfies the Verifier interface at
// compile time. A drift between the interface and the implementation
// would otherwise be caught only by callers, several files away.
var _ Verifier = (*compositeVerifier)(nil)

// Claims is the subset of OIDC ID-token claims the broker actually consumes.
// Subject + Email + EmailVerified are required to mint. Groups is optional
// and used by the per-world group allowlist (Slice C.1); IdPs that don't
// surface groups in the ID token (or aren't configured to) leave it nil
// and the operator falls back to AllowDomains or per-email carve-outs.
// HD is Google's Workspace hosted-domain claim — set by Google from the
// Workspace tenant binding (the user cannot influence it), empty for
// consumer Gmail accounts. The broker-global OIDC.AllowDomains gate keys
// on HD because email-domain parsing can be confused by aliases /
// unverified secondaries, while HD is asserted by Google's signature.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Groups        []string
	HD            string
}

// ExchangeResult carries verified claims and IdP tokens from a completed
// OAuth2 code exchange into browser and device-flow responses.
type ExchangeResult struct {
	Claims      Claims
	RawIDToken  string
	AccessToken string
	Expiry      time.Time
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
	// Exchange swaps a callback authorization code for verified claims
	// plus the raw IdP tokens. It fetches the token from the IdP,
	// verifies the ID-token signature against the JWKS, and returns the
	// verified claims alongside the raw id_token + access_token so the
	// device-flow polling endpoint can forward them to the client.
	Exchange(ctx context.Context, code string) (ExchangeResult, error)
	// VerifyIDToken validates a bearer ID token presented on
	// /me/install and the MCP gateway. Same signature + claims
	// extraction as Exchange's final step, without the OAuth code swap.
	VerifyIDToken(ctx context.Context, raw string) (Claims, error)
}

// ErrNoIDToken is returned by Exchange when the IdP responds without an
// id_token (would be unusual for a properly-configured OIDC client but is
// worth surfacing explicitly rather than panicking on a nil cast).
var ErrNoIDToken = errors.New("broker: oidc token response missing id_token")

// ErrEmailUnverified is returned when the IdP did not assert
// email_verified=true. The broker refuses to mint for unverified emails
// because the world authorization decisions (allow.domains / allow.emails)
// are meaningless on an unverified address.
var ErrEmailUnverified = errors.New("broker: id_token email not verified")

type oidcVerifier struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewVerifier builds an OIDC Verifier from the broker's OIDCConfig.
// Takes a pointer because OIDCConfig grew to 80+ bytes after PR4's
// BrokerSigningKey addition (gocritic hugeParam threshold). The
// constructor performs OIDC discovery (one HTTP call to the issuer)
// so it can fail fast at broker startup rather than on the first
// user login.
//
// The library is provider-agnostic: any RFC-compliant OIDC IdP with
// discovery works. "google" is just the first one we've validated.
func NewVerifier(ctx context.Context, cfg *OIDCConfig) (Verifier, error) {
	// nil-guard: NewVerifier is an error-returning API; a nil cfg
	// should surface as a structured error rather than a constructor
	// panic deep in the OIDC stack. Belt-and-suspenders — production
	// wiring always passes &cfg.OIDC after LoadConfig, but tests and
	// future callers benefit from the explicit boundary.
	if cfg == nil {
		return nil, fmt.Errorf("broker: oidc config is required")
	}
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

func (v *oidcVerifier) Exchange(ctx context.Context, code string) (ExchangeResult, error) {
	tok, err := v.oauth.Exchange(ctx, code)
	if err != nil {
		return ExchangeResult{}, fmt.Errorf("broker: oauth exchange: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return ExchangeResult{}, ErrNoIDToken
	}
	claims, err := v.VerifyIDToken(ctx, rawIDToken)
	if err != nil {
		return ExchangeResult{}, err
	}
	return ExchangeResult{
		Claims:      claims,
		RawIDToken:  rawIDToken,
		AccessToken: tok.AccessToken,
		Expiry:      tok.Expiry,
	}, nil
}

func (v *oidcVerifier) VerifyIDToken(ctx context.Context, rawIDToken string) (Claims, error) {
	idTok, err := v.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Claims{}, fmt.Errorf("broker: verify id_token: %w", err)
	}
	var raw struct {
		Email         string   `json:"email"`
		EmailVerified bool     `json:"email_verified"`
		Groups        []string `json:"groups"`
		HD            string   `json:"hd"`
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
		Groups:        raw.Groups,
		HD:            raw.HD,
	}, nil
}

// compositeVerifier multiplexes id_token verification across the
// broker's own signing key (PR4 refresh-grant minted tokens) and the
// IdP's JWKS (device-code-completion tokens still IdP-signed,
// pre-rotation legacy tokens, etc.). On AuthCodeURL + Exchange the
// composite is a transparent pass-through to the IdP-backed
// Verifier — only VerifyIDToken multiplexes.
//
// Dispatch policy: try the broker key first because it never
// touches the network (kid match short-circuits or fails cheaply).
// On ErrIDTokenKidUnknown (the token wasn't minted by this broker)
// fall through to the IdP path. Any other broker-side verification
// error (signature fail, bad iss, expired) is terminal — we do not
// silently retry against the IdP, since a kid-matching token that
// fails downstream checks indicates a tampered or stale broker
// token, not an IdP token.
//
// idTokenSigner is required (non-nil) in production wiring; tests
// that don't exercise broker-signed tokens may pass nil and the
// composite degrades to a transparent pass-through.
type compositeVerifier struct {
	primary       Verifier
	idTokenSigner *IDTokenSigner
	brokerURL     string
	clock         func() time.Time
}

// newCompositeVerifier wraps an existing Verifier (the IdP-backed
// one) with the broker's own signer. brokerURL is the iss + aud the
// broker emits on its tokens; the verifier checks them on the
// broker-leg path.
func newCompositeVerifier(primary Verifier, signer *IDTokenSigner, brokerURL string) *compositeVerifier {
	return &compositeVerifier{
		primary:       primary,
		idTokenSigner: signer,
		brokerURL:     brokerURL,
		clock:         time.Now,
	}
}

func (c *compositeVerifier) AuthCodeURL(state string) string {
	return c.primary.AuthCodeURL(state)
}

func (c *compositeVerifier) Exchange(ctx context.Context, code string) (ExchangeResult, error) {
	return c.primary.Exchange(ctx, code)
}

func (c *compositeVerifier) VerifyIDToken(ctx context.Context, raw string) (Claims, error) {
	if c.idTokenSigner != nil {
		claims, err := c.idTokenSigner.VerifyIDToken(raw, c.brokerURL, c.clock())
		if err == nil {
			return claims, nil
		}
		if !errors.Is(err, ErrIDTokenKidUnknown) {
			// Broker recognized the kid as its own but the
			// signature / claims failed. Falling through to the
			// IdP path here would mask a real failure (e.g. a
			// tampered broker token sneaking in via IdP's JWKS).
			return Claims{}, err
		}
	}
	return c.primary.VerifyIDToken(ctx, raw)
}
