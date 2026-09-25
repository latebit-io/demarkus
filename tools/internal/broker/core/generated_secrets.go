package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// EnsuredValue is a persisted secret value and whether this process generated it.
type EnsuredValue struct {
	Value     []byte
	Generated bool
}

// EnsureSecretValue returns the value at ref, generating and storing one when
// absent. Create-only through the store's read-modify-write, so racing
// replicas converge on one value; rotation is delete + restart.
func EnsureSecretValue(ctx context.Context, store SecretStore, ref SecretRef, generate func() ([]byte, error)) (EnsuredValue, error) {
	var result EnsuredValue
	err := store.Mutate(ctx, ref, func(current []byte) ([]byte, error) {
		// Mutate retries on conflict; only the final attempt's outcome counts.
		if len(current) > 0 {
			result = EnsuredValue{Value: current}
			return current, nil
		}
		fresh, err := generate()
		if err != nil {
			return nil, err
		}
		result = EnsuredValue{Value: fresh, Generated: true}
		return fresh, nil
	})
	if err != nil {
		return EnsuredValue{}, fmt.Errorf("ensure %s: %w", ref, err)
	}
	return result, nil
}

// SigningKeyResult is the signer built from the persisted key and whether
// this process generated it.
type SigningKeyResult struct {
	Signer    *IDTokenSigner
	Generated bool
}

// EnsureSigningKey returns a signer over the persisted id_token key,
// generating a fresh ECDSA P-256 PKCS#8 PEM when absent.
func EnsureSigningKey(ctx context.Context, store SecretStore, ref SecretRef) (SigningKeyResult, error) {
	key, err := EnsureSecretValue(ctx, store, ref, GenerateSigningKeyPEM)
	if err != nil {
		return SigningKeyResult{}, err
	}
	signer, err := NewIDTokenSigner(key.Value)
	if err != nil {
		return SigningKeyResult{}, fmt.Errorf("signing key %s is invalid: %w", ref, err)
	}
	return SigningKeyResult{Signer: signer, Generated: key.Generated}, nil
}

// GenerateSigningKeyPEM is a fresh ECDSA P-256 key as PKCS#8 PEM, the shape
// NewIDTokenSigner accepts.
func GenerateSigningKeyPEM() ([]byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate P-256 key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal PKCS#8: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// GenerateCookieKey is 32 random bytes, base64-encoded as server.cookieKey expects.
func GenerateCookieKey() ([]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("generate cookie key: %w", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(raw[:])), nil
}
