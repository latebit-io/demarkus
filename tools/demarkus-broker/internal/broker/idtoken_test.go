package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

// generateTestSigningKey produces a fresh ECDSA P-256 private key
// encoded as PKCS#8 PEM. Used across every test that needs a
// broker-side signer; ephemeral per test so failures don't leak
// fixture key material into other tests.
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

// generateTestSigningKeySEC1 produces the same key in the legacy
// SEC1 ("EC PRIVATE KEY") PEM shape — exercises the alternative
// branch of NewIDTokenSigner that accepts both PKCS#8 and SEC1.
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

func TestIDTokenSignerSignVerifyRoundTrip(t *testing.T) {
	s := newTestIDTokenSigner(t)
	brokerURL := "https://broker.example.com"
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	claims := Claims{
		Subject:       "google|alice",
		Email:         "alice@example.com",
		EmailVerified: true,
		Groups:        []string{"eng", "ops"},
	}
	raw, err := s.Sign(claims, brokerURL, 15*time.Minute, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Compact JWS: three base64url segments separated by dots.
	if got := strings.Count(raw, "."); got != 2 {
		t.Errorf("token = %q has %d dots, want 2", raw, got)
	}

	got, err := s.VerifyIDToken(raw, brokerURL, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if got.Subject != claims.Subject {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.Email != claims.Email {
		t.Errorf("Email = %q", got.Email)
	}
	if !got.EmailVerified {
		t.Errorf("EmailVerified false")
	}
	if len(got.Groups) != 2 || got.Groups[0] != "eng" || got.Groups[1] != "ops" {
		t.Errorf("Groups = %v", got.Groups)
	}
}

func TestIDTokenSignerSignRejectsZeroTTL(t *testing.T) {
	s := newTestIDTokenSigner(t)
	if _, err := s.Sign(Claims{}, "https://b", 0, time.Now()); err == nil {
		t.Fatalf("zero ttl should error")
	}
	if _, err := s.Sign(Claims{}, "https://b", -time.Second, time.Now()); err == nil {
		t.Fatalf("negative ttl should error")
	}
}

func TestIDTokenSignerVerifyRejectsExpired(t *testing.T) {
	s := newTestIDTokenSigner(t)
	now := time.Now()
	raw, err := s.Sign(Claims{Subject: "u"}, "https://b", time.Minute, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// 10 minutes after exp + the default 1-minute leeway should
	// reliably trip ErrExpired.
	_, err = s.VerifyIDToken(raw, "https://b", now.Add(10*time.Minute))
	if err == nil {
		t.Fatalf("expired token verified ok, want error")
	}
}

func TestIDTokenSignerVerifyRejectsWrongIssuer(t *testing.T) {
	s := newTestIDTokenSigner(t)
	now := time.Now()
	raw, err := s.Sign(Claims{Subject: "u"}, "https://b1.example.com", time.Minute, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := s.VerifyIDToken(raw, "https://b2.example.com", now); err == nil {
		t.Fatalf("wrong issuer accepted")
	}
}

func TestIDTokenSignerVerifyRejectsForeignKid(t *testing.T) {
	// Token signed by signer A is presented to signer B; B must
	// return ErrIDTokenKidUnknown so the composite verifier knows
	// to fall through to the IdP path.
	a := newTestIDTokenSigner(t)
	b := newTestIDTokenSigner(t)
	if a.KeyID() == b.KeyID() {
		t.Fatalf("two ephemeral signers collided on kid — astronomically unlikely, test wiring broken")
	}
	now := time.Now()
	raw, err := a.Sign(Claims{Subject: "u"}, "https://b", time.Minute, now)
	if err != nil {
		t.Fatalf("Sign on A: %v", err)
	}
	_, err = b.VerifyIDToken(raw, "https://b", now)
	if !errors.Is(err, ErrIDTokenKidUnknown) {
		t.Fatalf("err = %v, want ErrIDTokenKidUnknown", err)
	}
}

func TestIDTokenSignerVerifyRejectsBadSignature(t *testing.T) {
	s := newTestIDTokenSigner(t)
	now := time.Now()
	raw, err := s.Sign(Claims{Subject: "u"}, "https://b", time.Minute, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Flip a character in the signature segment. Compact JWS is
	// "header.payload.sig"; flipping any byte in the trailing
	// segment breaks the signature without changing kid.
	parts := strings.SplitN(raw, ".", 3)
	if len(parts) != 3 {
		t.Fatalf("token format unexpected: %q", raw)
	}
	tampered := []byte(parts[2])
	if tampered[0] == 'A' {
		tampered[0] = 'B'
	} else {
		tampered[0] = 'A'
	}
	bad := parts[0] + "." + parts[1] + "." + string(tampered)
	if _, err := s.VerifyIDToken(bad, "https://b", now); err == nil {
		t.Fatalf("tampered signature accepted")
	}
}

func TestNewIDTokenSignerErrors(t *testing.T) {
	tests := []struct {
		name   string
		input  []byte
		errSub string
	}{
		{"empty", nil, "empty"},
		{"not PEM", []byte("not-pem-data"), "not PEM-encoded"},
		{"wrong block type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x01}}), "unexpected PEM block type"},
		{"non-ECDSA PKCS8", rsaTestKeyPEM(t), "want *ecdsa.PrivateKey"},
		{"P-384 curve", p384TestKeyPEM(t), "P-256 curve"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewIDTokenSigner(tt.input)
			if err == nil {
				t.Fatalf("expected error containing %q", tt.errSub)
			}
			if !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tt.errSub)
			}
		})
	}
}

func TestNewIDTokenSignerAcceptsSEC1(t *testing.T) {
	// Legacy SEC1 PEM ("EC PRIVATE KEY") is also a valid input.
	if _, err := NewIDTokenSigner(generateTestSigningKeySEC1(t)); err != nil {
		t.Fatalf("SEC1 rejected: %v", err)
	}
}

func TestIDTokenSignerPublicJWK(t *testing.T) {
	s := newTestIDTokenSigner(t)
	jwk := s.PublicJWK()
	if jwk.KeyID != s.KeyID() {
		t.Errorf("jwk.KeyID = %q, signer.KeyID = %q", jwk.KeyID, s.KeyID())
	}
	if jwk.Algorithm != string(idTokenSignatureAlgorithm) {
		t.Errorf("jwk.Algorithm = %q, want %q", jwk.Algorithm, idTokenSignatureAlgorithm)
	}
	if jwk.Use != "sig" {
		t.Errorf("jwk.Use = %q, want sig", jwk.Use)
	}
	if _, ok := jwk.Key.(*ecdsa.PublicKey); !ok {
		t.Errorf("jwk.Key type = %T, want *ecdsa.PublicKey", jwk.Key)
	}
	// PublicJWK MUST NOT carry the private half.
	if !jwk.IsPublic() {
		t.Errorf("PublicJWK leaks private key material")
	}
}

// rsaTestKeyPEM returns a small RSA PKCS#8 PEM — only proving
// non-ECDSA-key rejection in NewIDTokenSigner. 1024 bits is well
// below production posture but the bit count is irrelevant here;
// the test asserts a type check, not crypto strength.
func rsaTestKeyPEM(t *testing.T) []byte {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa gen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal rsa: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func p384TestKeyPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-384: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal P-384: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
