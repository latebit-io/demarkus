package core

import (
	"context"
	"errors"
	"testing"
)

// memStore is a single-document SecretStore for EnsureSigningKey tests; err,
// when set, fails every call.
type memStore struct {
	data map[string][]byte
	err  error
}

func (m *memStore) Mutate(_ context.Context, ref SecretRef, mutate func([]byte) ([]byte, error)) error {
	if m.err != nil {
		return m.err
	}
	next, err := mutate(m.data[ref.String()])
	if err != nil {
		return err
	}
	if len(next) > 0 {
		m.data[ref.String()] = next
	}
	return nil
}

func (m *memStore) Delete(_ context.Context, ref SecretRef) error {
	if m.err != nil {
		return m.err
	}
	delete(m.data, ref.String())
	return nil
}

func TestEnsureSigningKeyGeneratesOnceThenReuses(t *testing.T) {
	store := &memStore{data: map[string][]byte{}}
	ref := SecretRef{Namespace: "ns", Name: "signing", Key: SigningKeySecretKey}

	first, err := EnsureSigningKey(context.Background(), store, ref)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !first.Generated {
		t.Fatal("first call should generate")
	}

	second, err := EnsureSigningKey(context.Background(), store, ref)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Generated {
		t.Fatal("second call must reuse the stored key")
	}
	if first.Signer.KeyID() != second.Signer.KeyID() {
		t.Fatal("stored key changed between calls")
	}
}

func TestEnsureSigningKeyRejectsCorruptStoredKey(t *testing.T) {
	ref := SecretRef{Namespace: "ns", Name: "signing", Key: SigningKeySecretKey}
	store := &memStore{data: map[string][]byte{ref.String(): []byte("not a pem")}}
	if _, err := EnsureSigningKey(context.Background(), store, ref); err == nil {
		t.Fatal("corrupt stored key must fail, not be silently regenerated")
	}
}

func TestEnsureSigningKeyPropagatesStoreError(t *testing.T) {
	ref := SecretRef{Namespace: "ns", Name: "signing", Key: SigningKeySecretKey}
	store := &memStore{err: errors.New("boom")}
	if _, err := EnsureSigningKey(context.Background(), store, ref); err == nil || !errors.Is(err, store.err) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestSigningKeyRefFileBackend(t *testing.T) {
	cfg := &Config{Storage: StorageConfig{Backend: StorageBackendFile, Dir: "/state"}, Server: ServerConfig{SigningKeySecret: "x"}}
	if got := SigningKeyRef(cfg).Path; got != "/state/signing-key.pem" {
		t.Errorf("Path = %q", got)
	}
}

// racingStore replays Mutate the way a create conflict does: the first
// attempt sees nothing, the retry sees the key another replica wrote.
type racingStore struct{ winner []byte }

func (r *racingStore) Mutate(_ context.Context, _ SecretRef, mutate func([]byte) ([]byte, error)) error {
	if _, err := mutate(nil); err != nil {
		return err
	}
	_, err := mutate(r.winner)
	return err
}

func (r *racingStore) Delete(context.Context, SecretRef) error { return nil }

func TestEnsureSigningKeyReportsFinalAttempt(t *testing.T) {
	winner, err := GenerateSigningKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	want, err := NewIDTokenSigner(winner)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EnsureSigningKey(context.Background(), &racingStore{winner: winner}, SecretRef{Name: "signing"})
	if err != nil {
		t.Fatalf("EnsureSigningKey: %v", err)
	}
	if got.Generated {
		t.Error("Generated = true, but the retry adopted another replica's key")
	}
	if got.Signer.KeyID() != want.KeyID() {
		t.Error("signer is not the key the store kept")
	}
}
