package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

// Fixtures core's own tests need. brokertest carries the same shapes for
// the other packages; core cannot import it (cycle), so they are repeated.

func testConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:            ":0",
			CookieKey:       "dGVzdC1rZXktMTIzNDU2Nzg5MGFi",
			BrokerNamespace: "broker-ns",
			StateTTL:        5 * time.Minute,
		},
		OIDC: OIDCConfig{
			Issuer: "https://idp", ClientID: "c", ClientSecret: "s", RedirectURL: "r",
		},
		Worlds: []WorldConfig{
			{
				Name:         "team-a",
				Namespace:    "team-a",
				TokensSecret: "team-a-tokens",
				Allow:        AllowConfig{Domains: []string{"example.com"}},
				DefaultToken: TokenScope{
					Paths: []string{"/team-a/*"},
				},
			},
		},
	}
}

// fakeVerifier is a Verifier with scripted claims for the composite tests.
type fakeVerifier struct {
	AuthURL    string
	Claims     Claims
	RawIDToken string
	VerifyFn   func(raw string) (Claims, error)
}

func (f *fakeVerifier) AuthCodeURL(state string) string {
	return f.AuthURL + "?state=" + state
}

func (f *fakeVerifier) Exchange(context.Context, string) (ExchangeResult, error) {
	return ExchangeResult{Claims: f.Claims, RawIDToken: f.RawIDToken}, nil
}

func (f *fakeVerifier) VerifyIDToken(_ context.Context, raw string) (Claims, error) {
	if f.VerifyFn != nil {
		return f.VerifyFn(raw)
	}
	return f.Claims, nil
}

// generateTestSigningKey is a fresh ECDSA P-256 key as PKCS#8 PEM.
func generateTestSigningKey(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// generateTestSigningKeySEC1 is the same key in the legacy SEC1 shape.
func generateTestSigningKeySEC1(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func newTestIDTokenSigner(t *testing.T) *IDTokenSigner {
	t.Helper()
	s, err := NewIDTokenSigner(generateTestSigningKey(t))
	if err != nil {
		t.Fatalf("NewIDTokenSigner: %v", err)
	}
	return s
}
