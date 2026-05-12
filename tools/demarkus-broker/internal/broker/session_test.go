package broker

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	s, err := NewSigner(key)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestSignerRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	nonce, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	state := State{Nonce: nonce, ExpiresAt: time.Now().Add(5 * time.Minute)}
	token, err := s.Sign(state)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Nonce != state.Nonce {
		t.Errorf("nonce = %q, want %q", got.Nonce, state.Nonce)
	}
	if !got.ExpiresAt.Equal(state.ExpiresAt) {
		t.Errorf("expiresAt = %v, want %v", got.ExpiresAt, state.ExpiresAt)
	}
}

func TestSignerRejectsTampering(t *testing.T) {
	s := newTestSigner(t)
	state := State{Nonce: "abc", ExpiresAt: time.Now().Add(5 * time.Minute)}
	token, err := s.Sign(state)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// flip a single byte in the payload portion
	parts := strings.SplitN(token, ".", 2)
	tampered := parts[0][:len(parts[0])-1] + flipChar(parts[0][len(parts[0])-1]) + "." + parts[1]
	if _, err := s.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on tampered payload, got %v", err)
	}
	// flip a byte in the signature
	tamperedSig := parts[0] + "." + parts[1][:len(parts[1])-1] + flipChar(parts[1][len(parts[1])-1])
	if _, err := s.Verify(tamperedSig); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on tampered sig, got %v", err)
	}
}

func TestSignerRejectsExpired(t *testing.T) {
	s := newTestSigner(t)
	state := State{Nonce: "abc", ExpiresAt: time.Now().Add(-1 * time.Second)}
	token, err := s.Sign(state)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := s.Verify(token); !errors.Is(err, ErrExpiredState) {
		t.Errorf("expected ErrExpiredState, got %v", err)
	}
}

func TestSignerRejectsMalformed(t *testing.T) {
	s := newTestSigner(t)
	cases := []string{
		"",
		"no-dot-here",
		"only.one.dot.too.many",
		".",
		"abc.def",
	}
	for _, c := range cases {
		if _, err := s.Verify(c); err == nil {
			t.Errorf("Verify(%q) succeeded unexpectedly", c)
		}
	}
}

func TestNewSignerRejectsShortKey(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := NewSigner(short); err == nil {
		t.Error("expected error on short key, got nil")
	}
}

func TestNewSignerRejectsBadBase64(t *testing.T) {
	if _, err := NewSigner("not!!base64??"); err == nil {
		t.Error("expected error on invalid base64 key")
	}
}

// flipChar swaps a base64url character with another valid one; just enough
// to corrupt the encoding without breaking it into something Verify rejects
// on syntax before getting to the HMAC check.
func flipChar(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}
