package broker

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const validConfig = `
server:
  addr: ":8080"
  cookieKey: "dGVzdC1rZXk="
  brokerNamespace: demarkus-broker
  issuancesSecret: broker-issuances
oidc:
  issuer: https://accounts.google.com
  clientID: client-abc
  clientSecret: shh
  redirectURL: https://broker.example.com/auth/callback
worlds:
  - name: team-a
    namespace: team-a
    tokensSecret: team-a-tokens
    allow:
      domains: ["example.com"]
    defaultToken:
      paths: ["/team-a/*"]
      operations: ["read", "publish"]
      expiresAfter: 24h
`

const validWorldBlock = `worlds:
  - name: team-a
    namespace: team-a
    tokensSecret: team-a-tokens
    allow:
      domains: ["example.com"]
    defaultToken:
      paths: ["/team-a/*"]
      operations: ["read", "publish"]
      expiresAfter: 24h
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := writeFile(path, body); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  string
		validate func(*testing.T, *Config)
	}{
		{
			name: "valid",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if c.Server.Addr != ":8080" {
					t.Errorf("addr = %q", c.Server.Addr)
				}
				if c.Server.StateTTL != 5*time.Minute {
					t.Errorf("StateTTL default = %v, want 5m", c.Server.StateTTL)
				}
				if len(c.Worlds) != 1 || c.Worlds[0].Name != "team-a" {
					t.Errorf("worlds = %+v", c.Worlds)
				}
				if c.Worlds[0].DefaultToken.ExpiresAfter != 24*time.Hour {
					t.Errorf("expiresAfter = %v", c.Worlds[0].DefaultToken.ExpiresAfter)
				}
			},
		},
		{
			name:    "missing server.addr",
			body:    strings.Replace(validConfig, `addr: ":8080"`, `addr: ""`, 1),
			wantErr: "server.addr is required",
		},
		{
			name:    "missing oidc.clientID",
			body:    strings.Replace(validConfig, `clientID: client-abc`, `clientID: ""`, 1),
			wantErr: "oidc.clientID is required",
		},
		{
			name:    "no worlds",
			body:    strings.Replace(validConfig, validWorldBlock, "worlds: []\n", 1),
			wantErr: "at least one world is required",
		},
		{
			name:    "duplicate world name",
			body:    validConfig + "  - name: team-a\n    namespace: team-a\n    tokensSecret: team-a-tokens\n    defaultToken:\n      paths: [\"/x\"]\n      operations: [\"read\"]\n      expiresAfter: 1h\n",
			wantErr: `duplicate name "team-a"`,
		},
		{
			name:    "missing defaultToken.operations",
			body:    strings.Replace(validConfig, `operations: ["read", "publish"]`, `operations: []`, 1),
			wantErr: "defaultToken.operations is required",
		},
		{
			name:    "zero expiresAfter rejected",
			body:    strings.Replace(validConfig, "expiresAfter: 24h", "expiresAfter: 0s", 1),
			wantErr: "defaultToken.expiresAfter must be > 0",
		},
		{
			name:    "negative stateTTL rejected",
			body:    strings.Replace(validConfig, `addr: ":8080"`, "addr: \":8080\"\n  stateTTL: -1m", 1),
			wantErr: "server.stateTTL must be > 0",
		},
		{
			name:    "sweeper.interval over 24h rejected",
			body:    validConfig + "sweeper:\n  interval: 48h\n",
			wantErr: "sweeper.interval must be <= 24h0m0s",
		},
		{
			name:    "sweeper.interval negative rejected",
			body:    validConfig + "sweeper:\n  interval: -1m\n",
			wantErr: "sweeper.interval must be > 0",
		},
		{
			name: "sweeper.interval defaults to 5m when omitted",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if c.Sweeper.Interval != 5*time.Minute {
					t.Errorf("sweeper.interval default = %s, want 5m", c.Sweeper.Interval)
				}
				if c.Sweeper.LeaseName != "demarkus-broker-sweeper" {
					t.Errorf("sweeper.leaseName default = %q, want demarkus-broker-sweeper", c.Sweeper.LeaseName)
				}
				if c.Sweeper.Disabled {
					t.Error("sweeper.disabled = true, want false default")
				}
			},
		},
		{
			name:    "unknown field caught",
			body:    validConfig + "extraField: oops\n",
			wantErr: "field extraField not found",
		},
		{
			name: "allow.domains normalized to lowercase",
			body: strings.Replace(validConfig,
				`allow:
      domains: ["example.com"]`,
				`allow:
      domains: ["  Example.COM  ", "OTHER.example"]`, 1),
			validate: func(t *testing.T, c *Config) {
				got := c.Worlds[0].Allow.Domains
				want := []string{"example.com", "other.example"}
				if len(got) != len(want) {
					t.Fatalf("domains = %v, want %v", got, want)
				}
				for j, d := range got {
					if d != want[j] {
						t.Errorf("domains[%d] = %q, want %q (must be lowercased + trimmed at load)", j, d, want[j])
					}
				}
			},
		},
		{
			name: "allow.domains empty entry rejected",
			body: strings.Replace(validConfig,
				`allow:
      domains: ["example.com"]`,
				`allow:
      domains: ["example.com", ""]`, 1),
			wantErr: "allow.domains[1] is empty",
		},
		{
			name: "allow.emails normalized to lowercase",
			body: strings.Replace(validConfig,
				`allow:
      domains: ["example.com"]`,
				`allow:
      emails: ["  Alice@Example.COM  ", "BOB@example.com"]`, 1),
			validate: func(t *testing.T, c *Config) {
				got := c.Worlds[0].Allow.Emails
				want := []string{"alice@example.com", "bob@example.com"}
				if len(got) != len(want) {
					t.Fatalf("emails = %v, want %v", got, want)
				}
				for j, e := range got {
					if e != want[j] {
						t.Errorf("emails[%d] = %q, want %q (must be lowercased + trimmed at load)", j, e, want[j])
					}
				}
			},
		},
		{
			name: "allow.emails empty entry rejected",
			body: strings.Replace(validConfig,
				`allow:
      domains: ["example.com"]`,
				`allow:
      emails: ["alice@example.com", "   "]`, 1),
			wantErr: "allow.emails[1] is empty",
		},
		{
			name: "allow.groups accepted",
			body: strings.Replace(validConfig,
				`allow:
      domains: ["example.com"]`,
				`allow:
      domains: ["example.com"]
      groups: ["engineering", "ops"]`, 1),
			validate: func(t *testing.T, c *Config) {
				got := c.Worlds[0].Allow.Groups
				if len(got) != 2 || got[0] != "engineering" || got[1] != "ops" {
					t.Errorf("groups = %v, want [engineering ops]", got)
				}
			},
		},
		{
			name: "allow.groups normalized to lowercase",
			// Groups are lowercased+trimmed at load, same as domains and
			// emails. Match is case-insensitive because our target IdP
			// set treats group-name uniqueness case-insensitively;
			// see the AllowConfig.Groups doc for why.
			body: strings.Replace(validConfig,
				`allow:
      domains: ["example.com"]`,
				`allow:
      groups: ["  Engineering  ", " OPS"]`, 1),
			validate: func(t *testing.T, c *Config) {
				got := c.Worlds[0].Allow.Groups
				want := []string{"engineering", "ops"}
				if len(got) != len(want) {
					t.Fatalf("groups = %v, want %v", got, want)
				}
				for j, g := range got {
					if g != want[j] {
						t.Errorf("groups[%d] = %q, want %q (must be lowercased + trimmed at load)", j, g, want[j])
					}
				}
			},
		},
		{
			name: "allow.groups empty entry rejected",
			body: strings.Replace(validConfig,
				`allow:
      domains: ["example.com"]`,
				`allow:
      groups: ["engineering", "   "]`, 1),
			wantErr: "allow.groups[1] is empty",
		},
		{
			// Plan §6.2 Slice C.4 defaults: tokens 10/min burst 5,
			// login 20/min burst 5. Operator omitting the block
			// gets the production-safe values applied at validate
			// — same shape as sweeper.interval defaulting.
			name: "rateLimit defaults applied when block omitted",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if c.RateLimit.Disabled {
					t.Error("rateLimit.disabled = true, want false default")
				}
				if c.RateLimit.Tokens.PerMinute != 10 {
					t.Errorf("rateLimit.tokens.perMinute = %d, want 10", c.RateLimit.Tokens.PerMinute)
				}
				if c.RateLimit.Tokens.Burst != 5 {
					t.Errorf("rateLimit.tokens.burst = %d, want 5", c.RateLimit.Tokens.Burst)
				}
				if c.RateLimit.Login.PerMinute != 20 {
					t.Errorf("rateLimit.login.perMinute = %d, want 20", c.RateLimit.Login.PerMinute)
				}
				if c.RateLimit.Login.Burst != 5 {
					t.Errorf("rateLimit.login.burst = %d, want 5", c.RateLimit.Login.Burst)
				}
				if c.RateLimit.TrustForwardedFor {
					t.Error("rateLimit.trustForwardedFor = true, want false default")
				}
			},
		},
		{
			name: "rateLimit operator overrides honored",
			body: validConfig + "rateLimit:\n  tokens:\n    perMinute: 60\n    burst: 10\n  login:\n    perMinute: 120\n    burst: 20\n  trustForwardedFor: true\n",
			validate: func(t *testing.T, c *Config) {
				if c.RateLimit.Tokens.PerMinute != 60 || c.RateLimit.Tokens.Burst != 10 {
					t.Errorf("tokens = %+v, want 60/10", c.RateLimit.Tokens)
				}
				if c.RateLimit.Login.PerMinute != 120 || c.RateLimit.Login.Burst != 20 {
					t.Errorf("login = %+v, want 120/20", c.RateLimit.Login)
				}
				if !c.RateLimit.TrustForwardedFor {
					t.Error("trustForwardedFor not honored")
				}
			},
		},
		{
			// disabled=true skips default-filling and value
			// validation, so an operator who explicitly opts out
			// can omit the per-route knobs entirely.
			name: "rateLimit disabled bypasses field defaults",
			body: validConfig + "rateLimit:\n  disabled: true\n",
			validate: func(t *testing.T, c *Config) {
				if !c.RateLimit.Disabled {
					t.Error("disabled not honored")
				}
				if c.RateLimit.Tokens.PerMinute != 0 || c.RateLimit.Login.PerMinute != 0 {
					t.Errorf("perMinute filled in despite disabled=true: tokens=%d login=%d",
						c.RateLimit.Tokens.PerMinute, c.RateLimit.Login.PerMinute)
				}
			},
		},
		{
			name:    "rateLimit.tokens.perMinute negative rejected",
			body:    validConfig + "rateLimit:\n  tokens:\n    perMinute: -1\n    burst: 5\n",
			wantErr: "rateLimit.tokens.perMinute must be >= 1",
		},
		{
			name:    "rateLimit.tokens.burst negative rejected",
			body:    validConfig + "rateLimit:\n  tokens:\n    perMinute: 10\n    burst: -1\n",
			wantErr: "rateLimit.tokens.burst must be >= 1",
		},
		{
			name:    "rateLimit.login.perMinute negative rejected",
			body:    validConfig + "rateLimit:\n  login:\n    perMinute: -1\n    burst: 5\n",
			wantErr: "rateLimit.login.perMinute must be >= 1",
		},
		{
			name:    "rateLimit.login.burst negative rejected",
			body:    validConfig + "rateLimit:\n  login:\n    perMinute: 20\n    burst: -1\n",
			wantErr: "rateLimit.login.burst must be >= 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.body)
			cfg, err := LoadConfig(path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.validate != nil {
				tt.validate(t, cfg)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// OIDC_CLIENT_SECRET env-var override lets the chart-rendered config
// Secret keep clientSecret blank and source the real value from an
// externally-managed Kubernetes Secret (External Secrets / Sealed
// Secrets / Vault) mounted via secretKeyRef. The env var wins over the
// file value when both are set, and an empty env var is treated as
// unset so an accidentally cleared variable cannot silently blank the
// runtime value.
func TestLoadConfigOIDCClientSecretEnvOverride(t *testing.T) {
	tests := []struct {
		name     string
		fileVal  string
		setEnv   bool
		envVal   string
		wantErr  string
		wantCSec string
	}{
		{
			name:     "env overrides file value",
			fileVal:  "shh-from-file",
			setEnv:   true,
			envVal:   "shh-from-env",
			wantCSec: "shh-from-env",
		},
		{
			name:     "env supplies value when file is empty",
			fileVal:  "",
			setEnv:   true,
			envVal:   "shh-from-env",
			wantCSec: "shh-from-env",
		},
		{
			name:     "empty env does not clobber file value",
			fileVal:  "shh-from-file",
			setEnv:   true,
			envVal:   "",
			wantCSec: "shh-from-file",
		},
		{
			name:    "no env, no file value, validate rejects",
			fileVal: "",
			setEnv:  false,
			wantErr: "oidc.clientSecret is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv unsets on test cleanup so each row is isolated;
			// not calling Setenv when setEnv=false ensures the var is
			// unset for that case (parent process is not expected to
			// have it set during `go test`).
			if tt.setEnv {
				t.Setenv("OIDC_CLIENT_SECRET", tt.envVal)
			}
			body := strings.Replace(validConfig,
				"clientSecret: shh",
				"clientSecret: "+strconv.Quote(tt.fileVal), 1)
			cfg, err := LoadConfig(writeConfig(t, body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.OIDC.ClientSecret != tt.wantCSec {
				t.Errorf("ClientSecret = %q, want %q", cfg.OIDC.ClientSecret, tt.wantCSec)
			}
		})
	}
}
