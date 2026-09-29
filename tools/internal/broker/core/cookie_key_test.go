package core

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

func TestEnsureCookieKeyGeneratesOnceThenReuses(t *testing.T) {
	store := &memStore{data: map[string][]byte{}}
	ref := SecretRef{Namespace: "ns", Name: "cookie", Key: CookieKeySecretKey}

	first, err := EnsureCookieKey(context.Background(), store, ref)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !first.Generated {
		t.Fatal("first call should generate")
	}
	if raw, err := base64.StdEncoding.DecodeString(first.Key); err != nil || len(raw) != cookieKeyBytes {
		t.Fatalf("key %q is not %d base64 bytes: %v", first.Key, cookieKeyBytes, err)
	}

	second, err := EnsureCookieKey(context.Background(), store, ref)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Generated || second.Key != first.Key {
		t.Fatalf("second call = %+v, want the stored key %q", second, first.Key)
	}
}

func TestEnsureCookieKeyReportsFinalAttempt(t *testing.T) {
	store := &racingStore{winner: []byte("d2lubmVyLWtleS0xMjM0NTY3OA==")}
	got, err := EnsureCookieKey(context.Background(), store, SecretRef{})
	if err != nil {
		t.Fatalf("EnsureCookieKey: %v", err)
	}
	if got.Generated || got.Key != string(store.winner) {
		t.Fatalf("got %+v, want the winner's key, not generated", got)
	}
}

func TestEnsureCookieKeyPropagatesStoreError(t *testing.T) {
	store := &memStore{err: errors.New("boom")}
	if _, err := EnsureCookieKey(context.Background(), store, SecretRef{}); !errors.Is(err, store.err) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

func TestCookieKeyRefFileBackend(t *testing.T) {
	cfg := &Config{Storage: StorageConfig{Backend: StorageBackendFile, Dir: "/state"}, Server: ServerConfig{CookieKeySecret: "x"}}
	if got := CookieKeyRef(cfg).Path; got != "/state/cookie-key" {
		t.Errorf("Path = %q", got)
	}
}
