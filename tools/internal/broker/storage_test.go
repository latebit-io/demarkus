package broker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFileStoreCreateAndUpdate(t *testing.T) {
	dir := t.TempDir()
	ref := SecretRef{Path: filepath.Join(dir, "tokens.toml")}
	s := NewFileSecretStore()

	if err := s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
		if len(existing) != 0 {
			t.Errorf("existing = %q, want empty on absent file", existing)
		}
		return []byte("v1"), nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := mustRead(t, ref.Path); string(got) != "v1" {
		t.Fatalf("file = %q, want v1", got)
	}

	if err := s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
		return append(existing, []byte("-v2")...), nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := mustRead(t, ref.Path); string(got) != "v1-v2" {
		t.Errorf("file = %q, want v1-v2", got)
	}
}

func TestFileStoreAbsentStaysAbsent(t *testing.T) {
	dir := t.TempDir()
	ref := SecretRef{Path: filepath.Join(dir, "refresh_tokens.json")}
	s := NewFileSecretStore()

	if err := s.Mutate(context.Background(), ref, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatalf("no-op mutate: %v", err)
	}
	if _, err := os.Stat(ref.Path); !os.IsNotExist(err) {
		t.Errorf("file materialized on empty-result no-op: %v", err)
	}
}

func TestFileStoreNoRewriteOnEqual(t *testing.T) {
	dir := t.TempDir()
	ref := SecretRef{Path: filepath.Join(dir, "tokens.toml")}
	if err := os.WriteFile(ref.Path, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewFileSecretStore()
	if err := s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
		return existing, nil
	}); err != nil {
		t.Fatalf("equal mutate: %v", err)
	}
	after, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Unchanged value must not be rewritten: the world server's tokens-file
	// watcher would reload on every no-op otherwise. SameFile catches a
	// rewrite that lands within the filesystem's mtime resolution.
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("file rewritten on unchanged value")
	}
	if !os.SameFile(before, after) {
		t.Error("file replaced (new inode) on unchanged value")
	}
}

func TestFileStorePreservesMode(t *testing.T) {
	dir := t.TempDir()
	ref := SecretRef{Path: filepath.Join(dir, "tokens.toml")}
	if err := os.WriteFile(ref.Path, []byte("v0"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := NewFileSecretStore()
	if err := s.Mutate(context.Background(), ref, func([]byte) ([]byte, error) {
		return []byte("v1"), nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640 (install.sh group-read preserved)", info.Mode().Perm())
	}
}

func TestFileStoreMutateErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	ref := SecretRef{Path: filepath.Join(dir, "f")}
	s := NewFileSecretStore()
	boom := errors.New("boom")
	if err := s.Mutate(context.Background(), ref, func([]byte) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if _, statErr := os.Stat(ref.Path); !os.IsNotExist(statErr) {
		t.Error("file written despite mutate error")
	}
}

func TestFileStoreMissingPath(t *testing.T) {
	s := NewFileSecretStore()
	err := s.Mutate(context.Background(), SecretRef{Namespace: "ns", Name: "n", Key: "k"}, func([]byte) ([]byte, error) {
		return []byte("x"), nil
	})
	if err == nil {
		t.Fatal("expected error for ref without path")
	}
}

func TestFileStoreConcurrentMutations(t *testing.T) {
	dir := t.TempDir()
	ref := SecretRef{Path: filepath.Join(dir, "counter")}
	s := NewFileSecretStore()

	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			errs <- s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
				return append(existing, 'x'), nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Mutate: %v", err)
		}
	}
	got, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Per-path locking makes read-modify-write atomic: no lost updates.
	if len(got) != n {
		t.Errorf("len = %d, want %d (lost updates under concurrency)", len(got), n)
	}
}

// testStorageDir keeps the fixture and every assertion on one literal.
const testStorageDir = "/var/lib/demarkus-knowledge-broker"

func TestConfigRefsFileMode(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{Backend: storageBackendFile, Dir: testStorageDir},
		Server:  ServerConfig{RefreshTokensSecret: "rt"},
	}
	world := &WorldConfig{Name: "soul", TokensFile: "/etc/demarkus/tokens.toml"}

	if got := cfg.refreshTokensRef().Path; got != testStorageDir+"/"+RefreshTokensSecretKey {
		t.Errorf("refresh ref path = %q", got)
	}
	if got := cfg.worldWriteTokenRef("soul").Path; got != testStorageDir+"/demarkus-broker-write-token-soul.json" {
		t.Errorf("write-token ref path = %q", got)
	}
	if got := cfg.worldTokensRef(world).Path; got != "/etc/demarkus/tokens.toml" {
		t.Errorf("world tokens ref path = %q", got)
	}

	// kubernetes mode: no file paths on broker-state refs.
	cfg.Storage = StorageConfig{}
	if got := cfg.refreshTokensRef().Path; got != "" {
		t.Errorf("k8s-mode refresh ref path = %q, want empty", got)
	}
}

// fileBackendConfig renders validConfig re-shaped for single-host mode:
// storage block added, brokerNamespace dropped, the world addressed by
// file + dial address instead of namespace + Secret.
func fileBackendConfig(mutate func(string) string) string {
	body := strings.Replace(validConfig, "  brokerNamespace: demarkus-knowledge-broker\n", "", 1)
	body = strings.Replace(body, "server:\n", "storage:\n  backend: file\n  dir: "+testStorageDir+"\nserver:\n", 1)
	body = strings.Replace(body,
		"  - name: team-a\n    namespace: team-a\n    tokensSecret: team-a-tokens\n",
		"  - name: team-a\n    tokensFile: /etc/demarkus/tokens.toml\n    internalAddress: localhost:6309\n", 1)
	if mutate != nil {
		body = mutate(body)
	}
	return body
}

func TestValidateFileBackend(t *testing.T) {
	if _, err := LoadConfig(writeConfig(t, fileBackendConfig(nil))); err != nil {
		t.Fatalf("valid file-backend config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"missing storage dir", func(s string) string {
			return strings.Replace(s, "  dir: "+testStorageDir+"\n", "", 1)
		}},
		{"missing world tokensFile", func(s string) string {
			return strings.Replace(s, "    tokensFile: /etc/demarkus/tokens.toml\n", "", 1)
		}},
		{"missing world internalAddress", func(s string) string {
			return strings.Replace(s, "    internalAddress: localhost:6309\n", "", 1)
		}},
		{"unknown backend", func(s string) string {
			return strings.Replace(s, "  backend: file\n", "  backend: etcd\n", 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, fileBackendConfig(tt.mutate))); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestValidateKubernetesBackendDefaults(t *testing.T) {
	// The unmodified valid config carries no storage block: backend must
	// default to kubernetes and keep requiring the namespace fields.
	cfg, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("valid k8s config rejected: %v", err)
	}
	if cfg.Storage.Backend != storageBackendKubernetes {
		t.Errorf("backend not defaulted, got %q", cfg.Storage.Backend)
	}
	broken := strings.Replace(validConfig, "  brokerNamespace: demarkus-knowledge-broker\n", "", 1)
	if _, err := LoadConfig(writeConfig(t, broken)); err == nil {
		t.Error("k8s mode without brokerNamespace should fail")
	}
}

func TestValidateFileBackendPathCollisions(t *testing.T) {
	tests := []struct {
		name       string
		tokensFile string
	}{
		{"aliases refresh state", testStorageDir + "/refresh_tokens.json"},
		{"aliases write-token state", testStorageDir + "/demarkus-broker-write-token-team-a.json"},
		{"aliases via non-clean path", testStorageDir + "/../demarkus-knowledge-broker/refresh_tokens.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fileBackendConfig(func(s string) string {
				return strings.Replace(s, "    tokensFile: /etc/demarkus/tokens.toml\n",
					"    tokensFile: "+tt.tokensFile+"\n", 1)
			})
			if _, err := LoadConfig(writeConfig(t, body)); err == nil {
				t.Error("expected collision error")
			}
		})
	}
}
