package broker

import (
	"path/filepath"
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
