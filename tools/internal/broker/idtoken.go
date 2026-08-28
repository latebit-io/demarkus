package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// idTokenSignatureAlgorithm pins broker-signed id_tokens to ECDSA
// P-256 with SHA-256 (RFC 7518 ES256). Choice rationale:
//   - Mandatory in OIDC core (every conformant client library
//     implements it).
//   - Compact wire format (~64-byte signature) keeps refresh
//     responses small on slow links.
//   - No PKCS#1 v1.5 footguns; deterministic-k variants are widely
//     audited.
//
// Pinned as a constant so the verifier's allowlist and the signer's
// emission can never drift.
const idTokenSignatureAlgorithm = jose.ES256

// defaultIDTokenTTL is the lifetime applied to broker-signed
// id_tokens when ServerConfig.IDTokenTTL is omitted. 15 minutes
// balances two pulls: short enough that the polling client refreshes
// within a working session boundary (compromise window stays
// bounded), long enough that PR5's /me/install request can complete
// without racing the expiry. Operators tighten via config.
const defaultIDTokenTTL = 15 * time.Minute

// ErrIDTokenKidUnknown is returned by IDTokenSigner.VerifyIDToken
// when the JWT header's `kid` is not the broker's current signing
// kid. Callers (the compositeVerifier) translate this into a
// fall-through to the IdP-signed verification path — the token was
// not minted by this broker, so the broker's public key cannot
// validate it.
var ErrIDTokenKidUnknown = errors.New("broker: id_token kid unknown to broker signer")

// IDTokenSigner mints and verifies the broker-signed JWTs returned
// by /device/token on the refresh-grant path (PR4). One signer per
// broker process, configured from a PEM-encoded ECDSA P-256 private
// key supplied via OIDCConfig.BrokerSigningKey.
//
// The signer also owns the JWK published at
// /.well-known/jwks.json (PublicJWK) — keeping mint and publish on
// the same type guarantees the kid the broker signs with is the kid
// it advertises.
//
// Key rotation is out of scope for PR4: there is one key, one kid,
// and rotation requires a config bump + broker restart. The
// roadmap calls for a published-old + published-new transition
// window once the operational pain becomes real.
type IDTokenSigner struct {
	priv   *ecdsa.PrivateKey
	pub    *ecdsa.PublicKey
	kid    string
	signer jose.Signer
}

// brokerIDTokenClaims is the broker's local claims shape — what we
// emit on Sign and unmarshal on Verify. Embeds the standard JWT
// registered claims (iss, sub, aud, exp, iat) and adds the OIDC
// email/groups extensions the broker propagates from the cached
// identity. JSON tags pin the wire names to the IETF spec; the
// nested jwt.Claims handles its own marshaling.
type brokerIDTokenClaims struct {
	jwt.Claims
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified,omitempty"`
	Groups        []string `json:"groups,omitempty"`
	HD            string   `json:"hd,omitempty"`
}

// NewIDTokenSigner parses the PEM-encoded private key and prepares
// the underlying jose.Signer. The PEM must be PKCS#8 — the standard
// OpenSSL output (`openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256`).
// SEC1 (`-----BEGIN EC PRIVATE KEY-----`) is also accepted because
// some operator tooling still emits it. Anything else fails fast.
//
// kid is derived from the SHA-256 hash of the marshaled public key
// (RFC 7638-flavored — not exact JWK thumbprint, but stable across
// restarts and unique to the keypair). Truncated to 16 hex chars to
// keep JWT headers compact.
func NewIDTokenSigner(pemBytes []byte) (*IDTokenSigner, error) {
	if len(pemBytes) == 0 {
		return nil, errors.New("broker: id_token signing key is empty")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("broker: id_token signing key is not PEM-encoded")
	}
	var priv *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("broker: parse PKCS#8 private key: %w", err)
		}
		ec, ok := anyKey.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("broker: PKCS#8 key is %T, want *ecdsa.PrivateKey", anyKey)
		}
		priv = ec
	case "EC PRIVATE KEY":
		ec, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("broker: parse SEC1 EC private key: %w", err)
		}
		priv = ec
	default:
		return nil, fmt.Errorf("broker: unexpected PEM block type %q (want PRIVATE KEY or EC PRIVATE KEY)", block.Type)
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("broker: id_token signing key must use P-256 curve (got %s)", priv.Curve.Params().Name)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("broker: marshal public key: %w", err)
	}
	sum := sha256.Sum256(pubDER)
	kid := base64.RawURLEncoding.EncodeToString(sum[:8])
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: idTokenSignatureAlgorithm, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		return nil, fmt.Errorf("broker: build jose signer: %w", err)
	}
	return &IDTokenSigner{
		priv:   priv,
		pub:    &priv.PublicKey,
		kid:    kid,
		signer: signer,
	}, nil
}

// Sign produces a fresh broker-signed id_token for the given claims.
// The token's iss + aud are the broker's PublicURL (the issuer the
// /.well-known/openid-configuration override advertises) so a
// strict client validates iss against the discovery doc and aud
// against itself. ttl must be > 0; callers pass
// ServerConfig.IDTokenTTL.
//
// Returns the compact JWS serialization (three base64url segments
// separated by dots) — the standard JWT wire format.
func (s *IDTokenSigner) Sign(claims *Claims, brokerURL string, ttl time.Duration, now time.Time) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("broker: id_token ttl must be > 0 (got %s)", ttl)
	}
	c := brokerIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   brokerURL,
			Subject:  claims.Subject,
			Audience: jwt.Audience{brokerURL},
			Expiry:   jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt: jwt.NewNumericDate(now),
		},
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Groups:        claims.Groups,
		HD:            claims.HD,
	}
	raw, err := jwt.Signed(s.signer).Claims(c).Serialize()
	if err != nil {
		return "", fmt.Errorf("broker: sign id_token: %w", err)
	}
	return raw, nil
}

// VerifyIDToken validates a broker-signed JWT against the broker's
// public key and standard claim invariants (iss == brokerURL, aud
// includes brokerURL, exp not in the past).
//
// Dispatch policy (consumed by compositeVerifier):
//
//   - Any failure BEFORE we confirm the broker kid match returns
//     ErrIDTokenKidUnknown — parse failure, missing headers, wrong
//     algorithm, kid mismatch. Those are all "not a broker-shaped
//     JWT" signals that should defer to the IdP-signed path.
//
//   - Any failure AFTER kid match is terminal — bad signature, bad
//     claims, expired. A kid-matching token that fails later checks
//     is either tampered or stale; falling through to the IdP path
//     would mask a real failure and let a forged token through if
//     the IdP's verifier was permissive.
//
// `now` is taken from a clock so tests can pin expiry behavior; the
// callers in compositeVerifier supply the wall clock.
func (s *IDTokenSigner) VerifyIDToken(raw, brokerURL string, now time.Time) (Claims, error) {
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{idTokenSignatureAlgorithm})
	if err != nil {
		// Malformed JWS, wrong algorithm, or just a non-JWT string —
		// none of which are broker-shaped tokens. Defer to IdP.
		return Claims{}, ErrIDTokenKidUnknown
	}
	if len(parsed.Headers) == 0 || parsed.Headers[0].KeyID != s.kid {
		return Claims{}, ErrIDTokenKidUnknown
	}
	var c brokerIDTokenClaims
	if err := parsed.Claims(s.pub, &c); err != nil {
		return Claims{}, fmt.Errorf("broker: verify id_token signature: %w", err)
	}
	err = c.ValidateWithLeeway(jwt.Expected{
		Issuer:      brokerURL,
		AnyAudience: jwt.Audience{brokerURL},
		Time:        now,
	}, jwt.DefaultLeeway)
	if err != nil {
		return Claims{}, fmt.Errorf("broker: validate id_token claims: %w", err)
	}
	return Claims{
		Subject:       c.Subject,
		Email:         c.Email,
		EmailVerified: c.EmailVerified,
		Groups:        c.Groups,
		HD:            c.HD,
	}, nil
}

// PublicJWK returns the JSON Web Key the broker advertises at
// /.well-known/jwks.json. Use="sig" + Algorithm="ES256" + the
// matching kid lets a strict OIDC client look up the right key from
// the JWKS by header `kid`. The private half is never exposed.
func (s *IDTokenSigner) PublicJWK() jose.JSONWebKey {
	return jose.JSONWebKey{
		Key:       s.pub,
		KeyID:     s.kid,
		Algorithm: string(idTokenSignatureAlgorithm),
		Use:       "sig",
	}
}

// KeyID returns the broker's signing kid. Surfaced so tests can
// assert header-level invariants without crafting a full JWT, and so
// future debugging logs can correlate broker-signed tokens back to
// the active key. Stable across the signer's lifetime.
func (s *IDTokenSigner) KeyID() string { return s.kid }
