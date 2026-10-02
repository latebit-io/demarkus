package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/mcpfmt"
)

// validConfigTemplate carries a sentinel where the broker signing
// key block goes. init() substitutes an ephemeral PEM generated
// per-test-binary so no checked-in PEM appears in the source tree
// (which would trigger secret scanners) and so parse-at-validate
// (added in PR4 review) actually sees a parseable key.
const validConfigTemplate = `
server:
  addr: ":8080"
  cookieKey: "dGVzdC1rZXk="
  brokerNamespace: demarkus-knowledge-broker
  stateBucket: gs://broker-state
  publicURL: "https://broker.example.com"
oidc:
  issuer: https://accounts.google.com
  clientID: client-abc
  clientSecret: shh
  redirectURL: https://broker.example.com/auth/callback
__SIGNING_KEY_BLOCK__
worlds:
  - name: team-a
    namespace: team-a
    allow:
      domains: ["example.com"]
    writeScope:
      paths: ["/team-a/*"]
`

// validConfig is the rendered template with a fresh signing-key
// block. Tests that exercise the "brokerSigningKey is required"
// path use validConfigNoSigningKey directly so they don't rely on
// brittle string replacements against the embedded PEM.
var (
	validConfigNoSigningKey string
)

func init() {
	pemBytes, err := GenerateSigningKeyPEM()
	if err != nil {
		panic("config_test: generate test signing key: " + err.Error())
	}
	// YAML literal block inside the `oidc:` map: brokerSigningKey
	// header at 2-space indent, PEM body at 4-space indent.
	var b strings.Builder
	b.WriteString("  brokerSigningKey: |\n")
	for line := range strings.SplitSeq(strings.TrimSpace(string(pemBytes)), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	validConfig = strings.Replace(validConfigTemplate, "__SIGNING_KEY_BLOCK__\n", b.String(), 1)
	validConfigNoSigningKey = strings.Replace(validConfigTemplate, "__SIGNING_KEY_BLOCK__\n", "", 1)
}

const validWorldBlock = `worlds:
  - name: team-a
    namespace: team-a
    allow:
      domains: ["example.com"]
    writeScope:
      paths: ["/team-a/*"]
`

// mustReplace guards fixture surgery: a stale needle would silently
// return the original config and the case would stop covering its branch.
func mustReplace(t *testing.T, s, old, replacement string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("fixture replacement missed: %q not present", old)
	}
	return strings.Replace(s, old, replacement, 1)
}

// withMemoryGateway turns the memory gateway on at its own host.
func withMemoryGateway(t *testing.T, body string) string {
	t.Helper()
	return mustReplace(t, body, `publicURL: "https://broker.example.com"`,
		"publicURL: \"https://broker.example.com\"\n  memory:\n    publicURL: \"https://memory.example.com\"")
}

func TestLoadConfig(t *testing.T) {
	clearConfigEnv(t)
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
			},
		},
		{
			name:    "missing server.stateBucket",
			body:    mustReplace(t, validConfig, "  stateBucket: gs://broker-state\n", ""),
			wantErr: "server.stateBucket",
		},
		{
			name:    "server.stateBucket without the gs:// scheme",
			body:    mustReplace(t, validConfig, "stateBucket: gs://broker-state", "stateBucket: broker-state"),
			wantErr: "must be a gs:// bucket URL",
		},
		{
			name:    "missing server.addr",
			body:    strings.Replace(validConfig, `addr: ":8080"`, `addr: ""`, 1),
			wantErr: "server.addr is required",
		},
		{
			name: "server.bearerAddr",
			body: strings.Replace(validConfig, `addr: ":8080"`, "addr: \":8080\"\n  bearerAddr: \":8443\"", 1),
			validate: func(t *testing.T, c *Config) {
				if c.Server.BearerAddr != ":8443" {
					t.Errorf("bearerAddr = %q, want :8443", c.Server.BearerAddr)
				}
			},
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
			name: "no worlds is legal with provisioning enabled",
			body: mustReplace(t, withMemoryGateway(t, validConfig), validWorldBlock,
				"worlds: []\nprovisioning:\n  mode: open\n  maxTenants: 10\n  authorityDomain: memory.svc\n  bucketPrefix: p-\n  bucketProject: p\n  worldsSecret: worlds\n"),
		},
		{
			name:    "write scope is required",
			body:    mustReplace(t, validConfig, "    writeScope:\n      paths: [\"/team-a/*\"]\n", ""),
			wantErr: "writeScope.paths is required",
		},
		{
			name:    "defaultToken is no longer accepted",
			body:    mustReplace(t, validConfig, "    writeScope:\n", "    defaultToken:\n"),
			wantErr: "field defaultToken not found",
		},
		{
			name: "a local world is served in process",
			body: mustReplace(t, validConfig, "    writeScope:\n", "    local: true\n    writeScope:\n"),
		},
		{
			name:    "brokerNamespace is required",
			body:    mustReplace(t, validConfig, "  brokerNamespace: demarkus-knowledge-broker\n", ""),
			wantErr: "server.brokerNamespace is required",
		},
		{
			name: "provisioning needs the memory gateway",
			body: mustReplace(t, validConfig, validWorldBlock,
				"worlds: []\nprovisioning:\n  mode: open\n  maxTenants: 10\n  authorityDomain: memory.svc\n  bucketPrefix: p-\n  bucketProject: p\n  worldsSecret: worlds\n"),
			wantErr: "needs the memory gateway",
		},
		{
			name:    "profile is required once the memory gateway is configured",
			body:    withMemoryGateway(t, validConfig),
			wantErr: "worlds[0] (team-a): profile is required",
		},
		{
			name: "memory world names its tenant",
			body: mustReplace(t, mustReplace(t, withMemoryGateway(t, validConfig), "name: team-a\n", "name: team-a\n    profile: memory\n"),
				"    allow:\n      domains: [\"example.com\"]\n", ""),
			wantErr: "allow must not be empty in a memory world",
		},
		{
			name:    "memory world needs the memory gateway",
			body:    mustReplace(t, validConfig, "name: team-a\n", "name: team-a\n    profile: memory\n"),
			wantErr: "profile memory needs the memory gateway",
		},
		{
			name:    "unknown profile",
			body:    mustReplace(t, validConfig, "name: team-a\n", "name: team-a\n    profile: soul\n"),
			wantErr: `profile must be "knowledge" or "memory"`,
		},
		{
			name: "memory gateway host must differ from the knowledge gateway's",
			body: mustReplace(t, validConfig, `publicURL: "https://broker.example.com"`,
				"publicURL: \"https://broker.example.com\"\n  memory:\n    publicURL: \"https://Broker.example.com/\""),
			wantErr: "server.memory.publicURL must be on a different host",
		},
		{
			name:    "duplicate world name",
			body:    validConfig + "  - name: team-a\n    namespace: team-a\n    writeScope:\n      paths: [\"/x\"]\n",
			wantErr: `duplicate name "team-a"`,
		},
		{
			name:    "uppercase world name",
			body:    strings.Replace(validConfig, "name: team-a", "name: Team-A", 1),
			wantErr: `worlds[0]: name "Team-A" must be a DNS label`,
		},
		{
			name:    "world name with a dot",
			body:    strings.Replace(validConfig, "name: team-a", "name: team.a", 1),
			wantErr: `must be a DNS label`,
		},
		{
			name:    "world name with a port",
			body:    strings.Replace(validConfig, "name: team-a", `name: "team-a:7000"`, 1),
			wantErr: `must be a DNS label`,
		},
		{
			name:    "world name ending in a hyphen",
			body:    strings.Replace(validConfig, "name: team-a", "name: team-", 1),
			wantErr: `must be a DNS label`,
		},
		{
			name:    "whitespace-only world namespace",
			body:    strings.Replace(validConfig, "namespace: team-a", `namespace: "   "`, 1),
			wantErr: "namespace or internalAddress is required",
		},
		{
			name: "an internal address needs no namespace",
			body: strings.Replace(validConfig, "namespace: team-a", `internalAddress: "team-a.example.com:6309"`, 1),
			validate: func(t *testing.T, c *Config) {
				if got := c.Worlds[0].Address(); got != "team-a.example.com:6309" {
					t.Errorf("Address() = %q, want the internal address", got)
				}
			},
		},
		{
			name:    "negative stateTTL rejected",
			body:    strings.Replace(validConfig, `addr: ":8080"`, "addr: \":8080\"\n  stateTTL: -1m", 1),
			wantErr: "server.stateTTL must be > 0",
		},
		{
			// YAML round-trip for the webClients registry — the chart
			// renders exactly this shape, and LoadConfig's
			// KnownFields(true) would reject a yaml-tag typo that the
			// direct validateWebClients tests can't catch.
			name: "webClients block parses and normalizes",
			body: validConfig + `webClients:
  - clientID: library-web
    clientSecretHash: "` + strings.ToUpper(strings.Repeat("ab", 32)) + `"
    redirectURIs: ["https://library.example.com/auth/callback"]
    name: Universe Library
`,
			validate: func(t *testing.T, c *Config) {
				if len(c.WebClients) != 1 {
					t.Fatalf("webClients = %+v", c.WebClients)
				}
				wc := c.WebClients[0]
				if wc.ClientID != "library-web" {
					t.Errorf("clientID = %q", wc.ClientID)
				}
				if wc.ClientSecretHash != strings.Repeat("ab", 32) {
					t.Errorf("hash not lowercased: %q", wc.ClientSecretHash)
				}
				if !wc.AllowsRedirect("https://library.example.com/auth/callback") {
					t.Errorf("redirectURIs = %v", wc.RedirectURIs)
				}
			},
		},
		{
			name: "webClients loopback redirect rejected at load",
			body: validConfig + `webClients:
  - clientID: library-web
    clientSecretHash: "` + strings.Repeat("ab", 32) + `"
    redirectURIs: ["https://localhost:8443/cb"]
`,
			wantErr: "loopback hosts",
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
			name: "oidc.allowDomains normalized to lowercase",
			body: strings.Replace(validConfig,
				`redirectURL: https://broker.example.com/auth/callback`,
				`redirectURL: https://broker.example.com/auth/callback
  allowDomains: ["  Latebit.IO  ", "NESTO.test"]`, 1),
			validate: func(t *testing.T, c *Config) {
				got := c.OIDC.AllowDomains
				want := []string{"latebit.io", "nesto.test"}
				if len(got) != len(want) {
					t.Fatalf("allowDomains = %v, want %v", got, want)
				}
				for j, d := range got {
					if d != want[j] {
						t.Errorf("allowDomains[%d] = %q, want %q (must be lowercased + trimmed at load)", j, d, want[j])
					}
				}
			},
		},
		{
			name: "oidc.allowDomains empty entry rejected",
			body: strings.Replace(validConfig,
				`redirectURL: https://broker.example.com/auth/callback`,
				`redirectURL: https://broker.example.com/auth/callback
  allowDomains: ["latebit.io", "   "]`, 1),
			wantErr: "oidc.allowDomains[1] is empty",
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
		{
			name: "worldDialer defaults to verify-strict",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if c.WorldDialer.InsecureSkipVerify {
					t.Error("worldDialer.insecureSkipVerify defaulted to true; want secure-by-default false")
				}
			},
		},
		{
			name: "worldDialer.insecureSkipVerify honored",
			body: validConfig + "worldDialer:\n  insecureSkipVerify: true\n",
			validate: func(t *testing.T, c *Config) {
				if !c.WorldDialer.InsecureSkipVerify {
					t.Error("worldDialer.insecureSkipVerify=true not honored")
				}
			},
		},
		{
			name:    "missing server.publicURL",
			body:    strings.Replace(validConfig, `publicURL: "https://broker.example.com"`, `publicURL: ""`, 1),
			wantErr: "server.publicURL is required",
		},
		{
			// Whitespace-only is functionally the same as empty —
			// normalize before the empty-check so a yaml-quoted
			// "   " value is rejected with the same error, not
			// silently allowed through to break downstream URL
			// construction.
			name:    "server.publicURL whitespace-only rejected",
			body:    strings.Replace(validConfig, `publicURL: "https://broker.example.com"`, `publicURL: "   "`, 1),
			wantErr: "server.publicURL is required",
		},
		{
			// Bare "/" trims down to "" after the canonicalization
			// pass and falls into the same is-required branch.
			name:    "server.publicURL bare slash rejected",
			body:    strings.Replace(validConfig, `publicURL: "https://broker.example.com"`, `publicURL: "/"`, 1),
			wantErr: "server.publicURL is required",
		},
		{
			// Without scheme + host the override would render
			// nonsense like "not-a-url/device/authorize" into the
			// discovery doc. Catch at config load.
			name:    "server.publicURL without scheme rejected",
			body:    strings.Replace(validConfig, `publicURL: "https://broker.example.com"`, `publicURL: "broker.example.com"`, 1),
			wantErr: "server.publicURL must be an absolute URL",
		},
		{
			// PublicURL trailing slash is stripped at load so every
			// downstream consumer (discovery doc, /me/install) sees a
			// canonical form without re-trimming on every call.
			name: "server.publicURL trailing slash stripped",
			body: strings.Replace(validConfig,
				`publicURL: "https://broker.example.com"`,
				`publicURL: "https://broker.example.com/"`, 1),
			validate: func(t *testing.T, c *Config) {
				if c.Server.PublicURL != "https://broker.example.com" {
					t.Errorf("PublicURL = %q, want trailing slash stripped", c.Server.PublicURL)
				}
			},
		},
		{
			// mcp.publicURL empty falls back to the issuer host so
			// single-host deployments need no new config.
			name: "server.mcp.publicURL defaults to server.publicURL",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if got := c.Server.Gateway(ProfileKnowledge).PublicURL; got != c.Server.PublicURL {
					t.Errorf("knowledge gateway URL = %q, want fallback to %q", got, c.Server.PublicURL)
				}
			},
		},
		{
			// Split-host: gateway host set explicitly, trailing slash
			// stripped like server.publicURL.
			name: "server.mcp.publicURL honored and normalized",
			body: strings.Replace(validConfig,
				`publicURL: "https://broker.example.com"`,
				"publicURL: \"https://broker.example.com\"\n  mcp:\n    publicURL: \"https://gateway.example.com/\"", 1),
			validate: func(t *testing.T, c *Config) {
				if c.Server.MCP.PublicURL != "https://gateway.example.com" {
					t.Errorf("MCP.PublicURL = %q, want https://gateway.example.com", c.Server.MCP.PublicURL)
				}
			},
		},
		{
			name: "server.mcp.publicURL without scheme rejected",
			body: strings.Replace(validConfig,
				`publicURL: "https://broker.example.com"`,
				"publicURL: \"https://broker.example.com\"\n  mcp:\n    publicURL: \"gateway.example.com\"", 1),
			wantErr: "server.mcp.publicURL must be an absolute URL",
		},
		{
			// A query or fragment would corrupt the string-appended
			// metadata URLs (resource, resource_metadata).
			name: "server.mcp.publicURL with query rejected",
			body: strings.Replace(validConfig,
				`publicURL: "https://broker.example.com"`,
				"publicURL: \"https://broker.example.com\"\n  mcp:\n    publicURL: \"https://gateway.example.com?x=1\"", 1),
			wantErr: "server.mcp.publicURL must not carry a query or fragment",
		},
		{
			name: "server.mcp.publicURL with fragment rejected",
			body: strings.Replace(validConfig,
				`publicURL: "https://broker.example.com"`,
				"publicURL: \"https://broker.example.com\"\n  mcp:\n    publicURL: \"https://gateway.example.com#frag\"", 1),
			wantErr: "server.mcp.publicURL must not carry a query or fragment",
		},
		{
			name: "server.publicURL with query rejected",
			body: strings.Replace(validConfig,
				`publicURL: "https://broker.example.com"`,
				`publicURL: "https://broker.example.com?x=1"`, 1),
			wantErr: "server.publicURL must not carry a query or fragment",
		},
		{
			// publicURL is optional — when omitted (validConfig), it
			// must round-trip to the zero value. /me/install will
			// skip worlds whose PublicURL is blank.
			name: "publicURL defaults to empty string when omitted",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if c.Worlds[0].PublicURL != "" {
					t.Errorf("PublicURL = %q, want empty default", c.Worlds[0].PublicURL)
				}
			},
		},
		{
			// publicURL is parsed as a plain string here; shape
			// validation lives in the /me/install consumer (PR5) so
			// PR1 only asserts the field round-trips through YAML.
			name: "publicURL round-trips through YAML",
			body: strings.Replace(validConfig,
				"namespace: team-a",
				"namespace: team-a\n    publicURL: \"mark://team-a.cluster.local:6309\"", 1),
			validate: func(t *testing.T, c *Config) {
				want := "mark://team-a.cluster.local:6309"
				if c.Worlds[0].PublicURL != want {
					t.Errorf("PublicURL = %q, want %q", c.Worlds[0].PublicURL, want)
				}
			},
		},
		{
			// A blank key is generated at startup (EnsureSigningKey), so
			// load succeeds and the store name gets its default.
			name: "brokerSigningKey optional, signingKeySecret defaults",
			body: validConfigNoSigningKey,
			validate: func(t *testing.T, c *Config) {
				if c.OIDC.BrokerSigningKey != "" {
					t.Errorf("BrokerSigningKey = %q, want blank", c.OIDC.BrokerSigningKey)
				}
				if c.Server.SigningKeySecret != DefaultSigningKeySecret {
					t.Errorf("SigningKeySecret = %q, want %q", c.Server.SigningKeySecret, DefaultSigningKeySecret)
				}
			},
		},
		{
			// PR4 review: malformed PEM is now caught at LoadConfig
			// (parse-at-validate) instead of bubbling up later
			// from main.run's NewIDTokenSigner. Operator-facing
			// error references oidc.brokerSigningKey directly.
			name: "brokerSigningKey invalid PEM rejected",
			body: strings.Replace(validConfigTemplate, "__SIGNING_KEY_BLOCK__\n",
				"  brokerSigningKey: \"not-a-pem\"\n", 1),
			wantErr: "oidc.brokerSigningKey is invalid",
		},
		{
			// PR4: refreshTokenTTL defaults to 90 days when
			// omitted — matches plan §"refresh tokens at 90-day TTL."
			name: "refreshTokenTTL default applied",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				want := 90 * 24 * time.Hour
				if c.Server.RefreshTokenTTL != want {
					t.Errorf("RefreshTokenTTL = %s, want %s", c.Server.RefreshTokenTTL, want)
				}
			},
		},
		{
			name: "refreshTokenTTL operator override",
			body: strings.Replace(validConfig,
				"publicURL: \"https://broker.example.com\"",
				"publicURL: \"https://broker.example.com\"\n  refreshTokenTTL: 720h", 1),
			validate: func(t *testing.T, c *Config) {
				want := 720 * time.Hour
				if c.Server.RefreshTokenTTL != want {
					t.Errorf("RefreshTokenTTL = %s, want %s", c.Server.RefreshTokenTTL, want)
				}
			},
		},
		{
			name: "idTokenTTL default applied",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				want := 15 * time.Minute
				if c.Server.IDTokenTTL != want {
					t.Errorf("IDTokenTTL = %s, want %s", c.Server.IDTokenTTL, want)
				}
			},
		},
		{
			// idTokenTTL ≥ refreshTokenTTL is degenerate — the
			// refresh credential would expire before the bearer it
			// mints. validate() rejects.
			name: "idTokenTTL must be less than refreshTokenTTL",
			body: strings.Replace(validConfig,
				"publicURL: \"https://broker.example.com\"",
				"publicURL: \"https://broker.example.com\"\n  idTokenTTL: 100h\n  refreshTokenTTL: 50h", 1),
			wantErr: "idTokenTTL",
		},
		{
			name:    "refreshTokenTTL negative rejected",
			body:    strings.Replace(validConfig, "publicURL: \"https://broker.example.com\"", "publicURL: \"https://broker.example.com\"\n  refreshTokenTTL: -1h", 1),
			wantErr: "server.refreshTokenTTL must be > 0",
		},
		{
			name:    "idTokenTTL negative rejected",
			body:    strings.Replace(validConfig, "publicURL: \"https://broker.example.com\"", "publicURL: \"https://broker.example.com\"\n  idTokenTTL: -1m", 1),
			wantErr: "server.idTokenTTL must be > 0",
		},
		{
			name: "oauthStateSecret default applied",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if c.Server.OAuthStateSecret != DefaultOAuthStateSecret {
					t.Errorf("OAuthStateSecret = %q, want %q", c.Server.OAuthStateSecret, DefaultOAuthStateSecret)
				}
			},
		},
		{
			name: "maxSessionsPerUser default applied",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if c.Server.MaxSessionsPerUser != DefaultMaxSessionsPerUser {
					t.Errorf("MaxSessionsPerUser = %d, want %d", c.Server.MaxSessionsPerUser, DefaultMaxSessionsPerUser)
				}
			},
		},
		{
			name:    "maxSessionsPerUser negative rejected",
			body:    strings.Replace(validConfig, "publicURL: \"https://broker.example.com\"", "publicURL: \"https://broker.example.com\"\n  maxSessionsPerUser: -1", 1),
			wantErr: "server.maxSessionsPerUser must be > 0",
		},
		{
			name: "stateBucket resolves to its bucket name",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				if got := c.Server.StateBucketName(); got != "broker-state" {
					t.Errorf("StateBucketName() = %q, want broker-state", got)
				}
			},
		},
		{
			name: "refreshTokensSecret is no longer a setting",
			body: strings.Replace(validConfig,
				"publicURL: \"https://broker.example.com\"",
				"publicURL: \"https://broker.example.com\"\n  refreshTokensSecret: my-refresh-tokens", 1),
			wantErr: "refreshTokensSecret",
		},
		{
			// The federation deriver writes the hub under a grant: no token.
			name:    "a world's tokensSecret is no longer a setting",
			body:    strings.Replace(validConfig, "namespace: team-a", "namespace: team-a\n    tokensSecret: team-a-tokens", 1),
			wantErr: "tokensSecret",
		},
		{
			name:    "agentTokens is no longer a setting",
			body:    validConfig + "agentTokens:\n  - world: team-a\n    secret: team-a-token-values\n    key: admin\n",
			wantErr: "agentTokens",
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
			// Always Setenv to make the test deterministic regardless of
			// whether the parent CI shell happens to have OIDC_CLIENT_SECRET
			// exported. t.Setenv restores the previous value (including
			// "unset") on test cleanup. Empty value is treated as unset by
			// applyEnvOverrides, which is the property the setEnv=false
			// case is asserting.
			envVal := tt.envVal
			if !tt.setEnv {
				envVal = ""
			}
			t.Setenv("OIDC_CLIENT_SECRET", envVal)
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

func TestGatewayConfigValidate(t *testing.T) {
	tests := []struct {
		name        string
		gateway     GatewayConfig
		wantErr     string
		wantProfile string
	}{
		{name: "tool profile defaults to full", wantProfile: mcpfmt.ProfileFull},
		{name: "lean tool profile accepted", gateway: GatewayConfig{ToolProfile: "lean"}, wantProfile: mcpfmt.ProfileLean},
		{name: "unknown tool profile rejected", gateway: GatewayConfig{ToolProfile: "wide"}, wantErr: "server.mcp.toolProfile: unknown tool profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.gateway.validate("server.mcp")
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validate: unexpected error %v", err)
				}
				if tt.gateway.ToolProfile != tt.wantProfile {
					t.Errorf("validate: toolProfile = %q, want %q", tt.gateway.ToolProfile, tt.wantProfile)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validate: err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigMCPBlockValidatesAtLoad(t *testing.T) {
	clearConfigEnv(t)
	body := strings.Replace(validConfig,
		`publicURL: "https://broker.example.com"`,
		"publicURL: \"https://broker.example.com\"\n  mcp:\n    toolProfile: wide",
		1)
	_, err := LoadConfig(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "server.mcp.toolProfile") {
		t.Fatalf("err = %v, want the gateway block validated at load", err)
	}
}

func TestLoadConfigRejectsTheExtraListener(t *testing.T) {
	// One listener serves the management API and both gateways; the old
	// server.mcp.addr must fail loudly rather than be ignored.
	clearConfigEnv(t)
	body := strings.Replace(validConfig,
		`publicURL: "https://broker.example.com"`,
		"publicURL: \"https://broker.example.com\"\n  mcp:\n    addr: \":8081\"",
		1)
	if _, err := LoadConfig(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "field addr not found") {
		t.Fatalf("err = %v, want server.mcp.addr rejected", err)
	}
}

func TestLoadConfigMCPDefaultsAppliedWhenBlockOmitted(t *testing.T) {
	// Configs without an `mcp:` block keep working: the knowledge gateway
	// answers on the management listener at the issuer's URL.
	clearConfigEnv(t)
	cfg, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Server.Gateway(ProfileKnowledge).PublicURL; got != cfg.Server.PublicURL {
		t.Errorf("knowledge gateway URL = %q, want the issuer %q", got, cfg.Server.PublicURL)
	}
	if got := cfg.Server.Resources(); len(got) != 1 || got[0] != cfg.Server.PublicURL+MCPPath {
		t.Errorf("Resources = %v, want the knowledge gateway alone", got)
	}
	if cfg.Worlds[0].Profile != ProfileKnowledge {
		t.Errorf("worlds[0].Profile = %q, want the knowledge default", cfg.Worlds[0].Profile)
	}
}

var validConfig string

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := writeFile(path, body); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// WriteFile is a tiny helper for tests; keeps test imports clean of os
// boilerplate. The 0o600 mode matches what protocol/token writes elsewhere.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

func TestCanonicalResource(t *testing.T) {
	cases := map[string]string{
		"https://MCP.Example.com/mcp":   "https://mcp.example.com/mcp",
		"HTTPS://mcp.example.com/mcp/":  "https://mcp.example.com/mcp",
		"https://mcp.example.com:8443/": "https://mcp.example.com:8443",
	}
	for raw, want := range cases {
		got, err := CanonicalResource(raw)
		if err != nil || got != want {
			t.Errorf("CanonicalResource(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "mcp.example.com/mcp", "https://mcp.example.com/mcp#frag", "https://mcp.example.com/mcp?x=1"} {
		if _, err := CanonicalResource(raw); err == nil {
			t.Errorf("CanonicalResource(%q) accepted", raw)
		}
	}
}

func TestServerConfigResourcesNameEveryGateway(t *testing.T) {
	s := ServerConfig{PublicURL: "https://broker.example.com"}
	if got := s.Resources(); len(got) != 1 || got[0] != "https://broker.example.com/mcp" {
		t.Fatalf("Resources = %v, want the issuer's /mcp", got)
	}
	s.MCP.PublicURL = "https://mcp.example.com"
	s.Memory.PublicURL = "https://memory.example.com"
	got := s.Resources()
	if len(got) != 2 || got[0] != "https://mcp.example.com/mcp" || got[1] != "https://memory.example.com/mcp" {
		t.Fatalf("Resources = %v, want both gateways", got)
	}
	if s.Memory.Host() != "memory.example.com" {
		t.Errorf("Memory.Host = %q", s.Memory.Host())
	}
}
