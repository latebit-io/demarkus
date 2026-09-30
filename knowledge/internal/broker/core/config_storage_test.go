package core

import (
	"strings"
	"testing"
)

// testStorageDir keeps the fixture and every assertion on one literal.
const testStorageDir = "/var/lib/demarkus-knowledge-broker"

func TestConfigRefsFileMode(t *testing.T) {
	cfg := &Config{
		Storage: StorageConfig{Backend: StorageBackendFile, Dir: testStorageDir},
		Server:  ServerConfig{RefreshTokensSecret: "rt"},
	}
	world := &WorldConfig{Name: "memory", TokensFile: "/etc/demarkus/tokens.toml"}

	if got := RefreshTokensRef(cfg).Path; got != testStorageDir+"/"+RefreshTokensSecretKey {
		t.Errorf("refresh ref path = %q", got)
	}
	if got := WorldWriteTokenRef(cfg, "memory").Path; got != testStorageDir+"/demarkus-broker-write-token-memory.json" {
		t.Errorf("write-token ref path = %q", got)
	}
	if got := WorldTokensRef(world).Path; got != "/etc/demarkus/tokens.toml" {
		t.Errorf("world tokens ref path = %q", got)
	}

	// kubernetes mode: no file paths on broker-state refs.
	cfg.Storage = StorageConfig{}
	if got := RefreshTokensRef(cfg).Path; got != "" {
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
	if cfg.Storage.Backend != StorageBackendKubernetes {
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
