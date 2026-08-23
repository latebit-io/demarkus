package knowledgeconfig

import (
	"math"
	"strings"
	"testing"
	"time"
)

const validConfig = `version: 1
listen:
  address: ":6309"
  maxIncomingStreams: 128
  idleTimeout: 30s
health:
  address: ":8081"
tls:
  certFile: /run/demarkus/tls/tls.crt
  keyFile: /run/demarkus/tls/tls.key
worlds:
  - name: world-a
    authorities:
      - World-A.Example.com
      - world-a.knowledge.svc.cluster.local
    bucket:
      url: gs://deployment-world-a
      worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48
    auth:
      tokensFile: /run/demarkus/world-a/tokens.toml
    policy:
      path: /.well-known/demarkus/policy.md
    readOnly: false
    limits:
      maxConcurrentRequests: 32
      requestTimeout: 10s
      requestsPerSecond: 50
      burst: 100
`

func TestParseValidConfig(t *testing.T) {
	config, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.Listen.Address != ":6309" || config.Listen.MaxIncomingStreams != 128 || config.Listen.IdleTimeout != Duration(30*time.Second) {
		t.Fatalf("unexpected listen config: %+v", config.Listen)
	}
	if config.Worlds[0].Authorities[0] != "world-a.example.com" {
		t.Fatalf("authority was not normalized: %q", config.Worlds[0].Authorities[0])
	}
	if config.Worlds[0].Limits.RequestTimeout != Duration(10*time.Second) {
		t.Fatalf("request timeout: got %s", time.Duration(config.Worlds[0].Limits.RequestTimeout))
	}
}

func TestParseDefaults(t *testing.T) {
	minimal := `version: 1
tls:
  certFile: cert.pem
  keyFile: key.pem
worlds:
  - name: world-a
    authorities: [world-a.example.com]
    bucket:
      url: gs://deployment-world-a
      worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48
    auth:
      tokensFile: tokens.toml
`
	config, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	world := config.Worlds[0]
	if config.Listen != (ListenConfig{Address: ":6309", MaxIncomingStreams: 128, IdleTimeout: Duration(30 * time.Second)}) {
		t.Errorf("listen defaults: %+v", config.Listen)
	}
	if config.Health.Address != ":8081" {
		t.Errorf("health address: got %q", config.Health.Address)
	}
	if world.Policy.Path != defaultPolicyPath {
		t.Errorf("policy path: got %q", world.Policy.Path)
	}
	wantLimits := LimitsConfig{MaxConcurrentRequests: 32, RequestTimeout: Duration(10 * time.Second), RequestsPerSecond: 50, Burst: 100}
	if world.Limits != wantLimits {
		t.Errorf("limits defaults: got %+v, want %+v", world.Limits, wantLimits)
	}
}

func TestParseRejectsInvalidYAMLShape(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unknown top-level field", validConfig + "unexpected: true\n", "field unexpected not found"},
		{"unknown nested field", strings.Replace(validConfig, "  idleTimeout: 30s", "  idleTimeout: 30s\n  unexpected: true", 1), "field unexpected not found"},
		{"unknown world field", strings.Replace(validConfig, "    readOnly: false", "    readOnly: false\n    unexpected: true", 1), "field unexpected not found"},
		{"trailing document", validConfig + "---\nversion: 1\n", "multiple documents are not allowed"},
		{"malformed trailing document", validConfig + "---\n[", "parse trailing YAML"},
		{"numeric duration", strings.Replace(validConfig, "idleTimeout: 30s", "idleTimeout: 30", 1), "duration must be a string"},
		{"invalid duration", strings.Replace(validConfig, "requestTimeout: 10s", "requestTimeout: soon", 1), "invalid duration \"soon\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error: got %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateRequiredAndLimitFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"version", func(config *Config) { config.Version = 2 }, "version must be 1"},
		{"listen address", func(config *Config) { config.Listen.Address = "" }, "listen.address must not be empty"},
		{"incoming streams", func(config *Config) { config.Listen.MaxIncomingStreams = 0 }, "maxIncomingStreams must be positive"},
		{"idle timeout", func(config *Config) { config.Listen.IdleTimeout = -1 }, "idleTimeout must not be negative"},
		{"health address", func(config *Config) { config.Health.Address = "" }, "health.address must not be empty"},
		{"TLS certificate", func(config *Config) { config.TLS.CertFile = "" }, "tls.certFile is required"},
		{"TLS key", func(config *Config) { config.TLS.KeyFile = "" }, "tls.keyFile is required"},
		{"worlds", func(config *Config) { config.Worlds = nil }, "worlds must contain at least one"},
		{"world name", func(config *Config) { config.Worlds[0].Name = "" }, "worlds[0].name is required"},
		{"authorities", func(config *Config) { config.Worlds[0].Authorities = nil }, "authorities must contain at least one"},
		{"bucket URL", func(config *Config) { config.Worlds[0].Bucket.URL = "" }, "must be exactly gs://bucket"},
		{"world ID", func(config *Config) { config.Worlds[0].Bucket.WorldID = "not-a-uuid" }, "canonical lowercase UUID"},
		{"tokens file", func(config *Config) { config.Worlds[0].Auth.TokensFile = "" }, "auth.tokensFile is required"},
		{"policy path", func(config *Config) { config.Worlds[0].Policy.Path = "/policy/../policy.md" }, "canonical absolute path"},
		{"unsupported policy path", func(config *Config) { config.Worlds[0].Policy.Path = "/policy.md" }, "only \"/.well-known/demarkus/policy.md\" is supported"},
		{"concurrency", func(config *Config) { config.Worlds[0].Limits.MaxConcurrentRequests = 0 }, "maxConcurrentRequests must be positive"},
		{"request timeout", func(config *Config) { config.Worlds[0].Limits.RequestTimeout = -1 }, "requestTimeout must not be negative"},
		{"negative rate", func(config *Config) { config.Worlds[0].Limits.RequestsPerSecond = -1 }, "requestsPerSecond must not be negative"},
		{"non-finite rate", func(config *Config) { config.Worlds[0].Limits.RequestsPerSecond = math.Inf(1) }, "requestsPerSecond must be finite"},
		{"negative burst", func(config *Config) { config.Worlds[0].Limits.Burst = -1 }, "burst must not be negative"},
		{"zero burst", func(config *Config) { config.Worlds[0].Limits.Burst = 0 }, "burst must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := mustParse(t, validConfig)
			test.mutate(config)
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error: got %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestAuthorityValidation(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		wantErr   string
	}{
		{"empty", "", "authority is empty"},
		{"wildcard", "*.example.com", "wildcards are not allowed"},
		{"trailing dot", "world.example.com.", "trailing dots are not allowed"},
		{"port", "world.example.com:6309", "ports and IP literals are not allowed"},
		{"Unicode", "wörld.example.com", "unicode is not allowed"},
		{"IPv4", "192.0.2.1", "IP literals are not allowed"},
		{"IPv6", "2001:db8::1", "ports and IP literals are not allowed"},
		{"empty label", "world..example.com", "DNS labels must not be empty"},
		{"leading hyphen", "-world.example.com", "must start and end"},
		{"underscore", "world_name.example.com", "invalid character"},
		{"long label", strings.Repeat("a", 64) + ".example.com", "exceeds 63 bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := mustParse(t, validConfig)
			config.Worlds[0].Authorities[0] = test.authority
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error: got %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestBucketURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"wrong scheme", "https://bucket-name", "exactly gs://bucket"},
		{"path", "gs://bucket-name/path", "exactly gs://bucket"},
		{"query", "gs://bucket-name?x=1", "exactly gs://bucket"},
		{"fragment", "gs://bucket-name#x", "exactly gs://bucket"},
		{"empty fragment", "gs://bucket-name#", "exactly gs://bucket"},
		{"port", "gs://bucket-name:443", "exactly gs://bucket"},
		{"uppercase", "gs://Bucket-Name", "invalid GCS bucket name"},
		{"IP", "gs://192.0.2.1", "invalid GCS bucket name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := mustParse(t, validConfig)
			config.Worlds[0].Bucket.URL = test.value
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error: got %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestValidWorldIDIsVersionAgnostic(t *testing.T) {
	if !ValidWorldID("52b471f7-8d38-0c89-b44a-6f4f8b1a4f48") {
		t.Fatal("ValidWorldID rejected canonical version-agnostic world ID")
	}
	if ValidWorldID("52b471f7-8d38-4c89-f44a-6f4f8b1a4f48") {
		t.Fatal("ValidWorldID accepted non-RFC variant")
	}
}

func TestValidateDuplicateWorldIdentities(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*WorldConfig, *WorldConfig)
		wantErr string
	}{
		{"world name", func(first, second *WorldConfig) { second.Name = first.Name }, "duplicates worlds[0].name"},
		{"authority after case folding", func(first, second *WorldConfig) { second.Authorities[0] = strings.ToUpper(first.Authorities[0]) }, "duplicates authority in worlds[0]"},
		{"bucket URL", func(first, second *WorldConfig) { second.Bucket.URL = first.Bucket.URL }, "duplicates worlds[0].bucket.url"},
		{"world ID", func(first, second *WorldConfig) { second.Bucket.WorldID = first.Bucket.WorldID }, "duplicates worlds[0].bucket.worldID"},
		{"clean token file", func(_, second *WorldConfig) {
			second.Auth.TokensFile = "/run/demarkus/world-b/../world-a/tokens.toml"
		}, "duplicates worlds[0].auth.tokensFile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := mustParse(t, validConfig)
			config.Worlds = append(config.Worlds, secondWorld())
			test.mutate(&config.Worlds[0], &config.Worlds[1])
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error: got %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateAllowsDisabledRateAndTimeout(t *testing.T) {
	config := mustParse(t, validConfig)
	config.Worlds[0].Limits.RequestsPerSecond = 0
	config.Worlds[0].Limits.RequestTimeout = 0
	config.Listen.IdleTimeout = 0
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func mustParse(t *testing.T, body string) *Config {
	t.Helper()
	config, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return config
}

func secondWorld() WorldConfig {
	return WorldConfig{
		Name:        "world-b",
		Authorities: []string{"world-b.example.com"},
		Bucket: BucketConfig{
			URL:     "gs://deployment-world-b",
			WorldID: "6961a451-d91c-40b4-8f1a-dd1f8849f79f",
		},
		Auth:   AuthConfig{TokensFile: "/run/demarkus/world-b/tokens.toml"},
		Policy: PolicyConfig{Path: defaultPolicyPath},
		Limits: LimitsConfig{
			MaxConcurrentRequests: 32,
			RequestTimeout:        Duration(10 * time.Second),
			RequestsPerSecond:     50,
			Burst:                 100,
		},
	}
}
