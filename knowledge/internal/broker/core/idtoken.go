package core

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

// IDTokenSignatureAlgorithm pins broker signed id_tokens to ES256, the one
// algorithm OIDC core mandates; the signer and the verifier share it.
const IDTokenSignatureAlgorithm = jose.ES256

// DefaultIDTokenTTL applies when ServerConfig.IDTokenTTL is omitted: short
// enough to bound a leaked bearer, long enough for /me/install to finish.
const DefaultIDTokenTTL = 15 * time.Minute

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
		jose.SigningKey{Algorithm: IDTokenSignatureAlgorithm, Key: priv},
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

// Sign produces a broker-signed id_token: iss is the broker's PublicURL, aud
// is claims.Resource (RFC 8707) or, unbound, the broker URL.
// ttl must be > 0; callers pass ServerConfig.IDTokenTTL.
func (s *IDTokenSigner) Sign(claims *Claims, brokerURL string, ttl time.Duration, now time.Time) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("broker: id_token ttl must be > 0 (got %s)", ttl)
	}
	aud := jwt.Audience{brokerURL}
	if claims.Resource != "" {
		aud = jwt.Audience{claims.Resource}
	}
	c := brokerIDTokenClaims{
		Claims: jwt.Claims{
			Issuer:   brokerURL,
			Subject:  claims.Subject,
			Audience: aud,
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

// VerifyIDToken checks a broker-signed JWT: key, iss, one of audiences, exp.
// An aud other than the broker URL comes back as Claims.Resource. Failures
// before the kid matches are ErrIDTokenKidUnknown (defer to the IdP); after, terminal.
func (s *IDTokenSigner) VerifyIDToken(raw, brokerURL string, audiences []string, now time.Time) (Claims, error) {
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{IDTokenSignatureAlgorithm})
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
		AnyAudience: jwt.Audience(audiences),
		Time:        now,
	}, jwt.DefaultLeeway)
	if err != nil {
		return Claims{}, fmt.Errorf("broker: validate id_token claims: %w", err)
	}
	claims := Claims{
		Subject:       c.Subject,
		Email:         c.Email,
		EmailVerified: c.EmailVerified,
		Groups:        c.Groups,
		HD:            c.HD,
	}
	if !c.Audience.Contains(brokerURL) {
		claims.Resource = c.Audience[0]
	}
	if c.Expiry != nil {
		claims.Expiry = c.Expiry.Time()
	}
	return claims, nil
}

// PublicJWK returns the JSON Web Key the broker advertises at
// /.well-known/jwks.json. Use="sig" + Algorithm="ES256" + the
// matching kid lets a strict OIDC client look up the right key from
// the JWKS by header `kid`. The private half is never exposed.
func (s *IDTokenSigner) PublicJWK() jose.JSONWebKey {
	return jose.JSONWebKey{
		Key:       s.pub,
		KeyID:     s.kid,
		Algorithm: string(IDTokenSignatureAlgorithm),
		Use:       "sig",
	}
}

// KeyID returns the broker's signing kid. Surfaced so tests can
// assert header-level invariants without crafting a full JWT, and so
// future debugging logs can correlate broker-signed tokens back to
// the active key. Stable across the signer's lifetime.
func (s *IDTokenSigner) KeyID() string { return s.kid }
