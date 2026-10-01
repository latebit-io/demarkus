package oauthsrv

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// State is the HMAC-signed OIDC state cookie payload: CSRF nonce, expiry,
// and the flow /auth/callback resumes. Dispatch keys off these signed
// fields, never an ambient cookie.
type State struct {
	Nonce     string    `json:"n"`
	ExpiresAt time.Time `json:"e"`
	// DeviceKey is the device grant's key, its device_code's hash.
	DeviceKey string `json:"d,omitempty"`
	// AuthCode is the authorize request, carried here instead of stored.
	AuthCode *AuthCodeRequest `json:"a,omitempty"`
}

// Signer encodes and verifies State values into the signed cookie format
// "<base64url(payload)>.<base64url(HMAC)>". Construct one Signer per
// broker process from a config-supplied key.
type Signer struct {
	key []byte
}

// NewSigner builds a Signer from a base64-encoded HMAC key (core.DecodeCookieKey).
func NewSigner(b64Key string) (*Signer, error) {
	key, err := core.DecodeCookieKey(b64Key)
	if err != nil {
		return nil, fmt.Errorf("broker: %w", err)
	}
	return &Signer{key: key}, nil
}

// NewNonce returns a fresh 16-byte random nonce, hex-encoded. Used as
// the OAuth `state` parameter and stored inside the signed State cookie
// for cross-check on callback.
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("broker: nonce: %w", err)
	}
	return fmt.Sprintf("%x", b[:]), nil
}

// Sign encodes the State as JSON, base64url-encodes the payload, computes
// HMAC-SHA256 over the encoded payload, and returns "<payload>.<sig>".
func (s *Signer) Sign(state State) (string, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("broker: marshal state: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

// ErrInvalidSignature is returned when the cookie's HMAC does not verify
// against the broker's key.
var ErrInvalidSignature = errors.New("broker: invalid cookie signature")

// ErrExpiredState is returned when the cookie's ExpiresAt is in the past.
var ErrExpiredState = errors.New("broker: state cookie expired")

// Verify checks the HMAC, parses the payload, and rejects expired state.
// The signature is checked in constant time to avoid timing oracles.
func (s *Signer) Verify(token string) (State, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return State{}, ErrInvalidSignature
	}
	expected := hmac.New(sha256.New, s.key)
	expected.Write([]byte(parts[0]))
	expectedSig := base64.RawURLEncoding.EncodeToString(expected.Sum(nil))
	if !hmac.Equal([]byte(expectedSig), []byte(parts[1])) {
		return State{}, ErrInvalidSignature
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return State{}, fmt.Errorf("broker: decode state payload: %w", err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, fmt.Errorf("broker: parse state: %w", err)
	}
	if state.ExpiresAt.Before(time.Now()) {
		return state, ErrExpiredState
	}
	return state, nil
}
