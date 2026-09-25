package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// SigningKeyResult is the signer built from the persisted key and whether
// this process generated it.
type SigningKeyResult struct {
	Signer    *IDTokenSigner
	Generated bool
}

// EnsureSigningKey returns a signer over the persisted id_token key, generating
// a fresh ECDSA P-256 PKCS#8 PEM when absent. Create-only through the store's
// read-modify-write, so racing replicas converge; rotation is delete + restart.
func EnsureSigningKey(ctx context.Context, store SecretStore, ref SecretRef) (SigningKeyResult, error) {
	var pemBytes []byte
	generated := false
	err := store.Mutate(ctx, ref, func(current []byte) ([]byte, error) {
		// Mutate retries on conflict; only the final attempt's outcome counts.
		pemBytes, generated = nil, false
		if len(current) > 0 {
			pemBytes = current
			return current, nil
		}
		fresh, err := GenerateSigningKeyPEM()
		if err != nil {
			return nil, err
		}
		pemBytes, generated = fresh, true
		return fresh, nil
	})
	if err != nil {
		return SigningKeyResult{}, fmt.Errorf("ensure signing key %s: %w", ref, err)
	}
	signer, err := NewIDTokenSigner(pemBytes)
	if err != nil {
		return SigningKeyResult{}, fmt.Errorf("signing key %s is invalid: %w", ref, err)
	}
	return SigningKeyResult{Signer: signer, Generated: generated}, nil
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
