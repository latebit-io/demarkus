package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
    tokensSecret: team-a-tokens
    allow:
      domains: ["example.com"]
    defaultToken:
      paths: ["/team-a/*"]
`

// validConfig is the rendered template with a fresh signing-key
// block. Tests that exercise the "brokerSigningKey is required"
// path use validConfigNoSigningKey directly so they don't rely on
// brittle string replacements against the embedded PEM.
var (
	validConfig             string
	validConfigNoSigningKey string
)

func init() {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic("config_test: generate test signing key: " + err.Error())
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic("config_test: marshal test signing key: " + err.Error())
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
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
    tokensSecret: team-a-tokens
    allow:
      domains: ["example.com"]
    defaultToken:
      paths: ["/team-a/*"]
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
	// applyEnvOverrides reads BROKER_SIGNING_KEY and
	// OIDC_CLIENT_SECRET from the process environment. Without
	// clearing them, a CI runner that exports either var (or a
	// developer who set them locally) silently masks YAML-missing
	// test cases. t.Setenv restores the prior value at test end so
	// peer tests that DO want a real env value still work.
	t.Setenv("BROKER_SIGNING_KEY", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
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
			name: "no worlds is legal with provisioning enabled",
			body: strings.Replace(validConfig, validWorldBlock,
				"worlds: []\nprovisioning:\n  mode: open\n  maxTenants: 10\n  authorityDomain: memory.svc\n  dialAddress: memory.svc:6309\n  bucketPrefix: p-\n  bucketProject: p\n  serverNamespace: ns\n", 1),
		},
		{
			name:    "duplicate world name",
			body:    validConfig + "  - name: team-a\n    namespace: team-a\n    tokensSecret: team-a-tokens\n    defaultToken:\n      paths: [\"/x\"]\n",
			wantErr: `duplicate name "team-a"`,
		},
		{
			name:    "duplicate world tokens Secret reference",
			body:    validConfig + "  - name: team-b\n    namespace: team-a\n    tokensSecret: team-a-tokens\n    defaultToken:\n      paths: [\"/x\"]\n",
			wantErr: `duplicate tokens Secret reference "team-a/team-a-tokens"`,
		},
		{
			name:    "normalized duplicate world tokens Secret reference",
			body:    validConfig + "  - name: team-b\n    namespace: \" TEAM-A \"\n    tokensSecret: \" TEAM-A-TOKENS \"\n    defaultToken:\n      paths: [\"/x\"]\n",
			wantErr: `duplicate tokens Secret reference "team-a/team-a-tokens"`,
		},
		{
			name:    "whitespace-only world namespace",
			body:    strings.Replace(validConfig, "namespace: team-a", `namespace: "   "`, 1),
			wantErr: "namespace is required",
		},
		{
			name:    "whitespace-only world tokens Secret",
			body:    strings.Replace(validConfig, "tokensSecret: team-a-tokens", `tokensSecret: "   "`, 1),
			wantErr: "tokensSecret is required",
		},
		{
			name: "same tokens Secret name in different namespace",
			body: validConfig + "  - name: team-b\n    namespace: team-b\n    tokensSecret: team-a-tokens\n    defaultToken:\n      paths: [\"/x\"]\n",
			validate: func(t *testing.T, c *Config) {
				if len(c.Worlds) != 2 {
					t.Errorf("worlds = %+v", c.Worlds)
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
				if !wc.allowsRedirect("https://library.example.com/auth/callback") {
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
				if c.Server.MCP.PublicURL != c.Server.PublicURL {
					t.Errorf("MCP.PublicURL = %q, want fallback to %q", c.Server.MCP.PublicURL, c.Server.PublicURL)
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
				"tokensSecret: team-a-tokens",
				"tokensSecret: team-a-tokens\n    publicURL: \"mark://team-a.cluster.local:6309\"", 1),
			validate: func(t *testing.T, c *Config) {
				want := "mark://team-a.cluster.local:6309"
				if c.Worlds[0].PublicURL != want {
					t.Errorf("PublicURL = %q, want %q", c.Worlds[0].PublicURL, want)
				}
			},
		},
		{
			// PR4: brokerSigningKey is required so a refresh-grant
			// request never falls through to a "feature missing"
			// runtime error. validate() now parses the PEM too
			// (see OIDCConfig.validate doc), but missing-field
			// short-circuits before parse so the error message
			// matches the "is required" surface, not the parse
			// error.
			name:    "brokerSigningKey required",
			body:    validConfigNoSigningKey,
			wantErr: "oidc.brokerSigningKey is required",
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
			name: "refreshTokensSecret default applied",
			body: validConfig,
			validate: func(t *testing.T, c *Config) {
				want := defaultRefreshTokensSecret
				if c.Server.RefreshTokensSecret != want {
					t.Errorf("RefreshTokensSecret = %q, want %q", c.Server.RefreshTokensSecret, want)
				}
			},
		},
		{
			name: "refreshTokensSecret operator override",
			body: strings.Replace(validConfig,
				"publicURL: \"https://broker.example.com\"",
				"publicURL: \"https://broker.example.com\"\n  refreshTokensSecret: my-refresh-tokens", 1),
			validate: func(t *testing.T, c *Config) {
				if c.Server.RefreshTokensSecret != "my-refresh-tokens" {
					t.Errorf("RefreshTokensSecret = %q", c.Server.RefreshTokensSecret)
				}
			},
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

func TestMCPConfigValidate(t *testing.T) {
	tests := []struct {
		name     string
		mcp      MCPConfig
		wantErr  string
		wantAddr string
	}{
		{
			name:     "zero value gets default addr",
			mcp:      MCPConfig{},
			wantAddr: defaultMCPAddr,
		},
		{
			name:     "explicit addr preserved",
			mcp:      MCPConfig{Addr: ":9090"},
			wantAddr: ":9090",
		},
		{
			name:     "addr + both tls fields — valid",
			mcp:      MCPConfig{Addr: ":8081", TLS: MCPTLSConfig{CertFile: "/c", KeyFile: "/k"}},
			wantAddr: ":8081",
		},
		{
			name:    "tls cert without key",
			mcp:     MCPConfig{Addr: ":8081", TLS: MCPTLSConfig{CertFile: "/c"}},
			wantErr: "server.mcp.tls.certFile and server.mcp.tls.keyFile must be set together",
		},
		{
			name:    "tls key without cert",
			mcp:     MCPConfig{Addr: ":8081", TLS: MCPTLSConfig{KeyFile: "/k"}},
			wantErr: "server.mcp.tls.certFile and server.mcp.tls.keyFile must be set together",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mcp.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validate: unexpected error %v", err)
				}
				if tt.mcp.Addr != tt.wantAddr {
					t.Errorf("validate: addr = %q, want %q", tt.mcp.Addr, tt.wantAddr)
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
	// One YAML-round-trip case to pin that the new MCPConfig.validate
	// hook is actually called from Config.validate() — a unit test on
	// MCPConfig.validate alone wouldn't catch a missing hook in the
	// outer validation chain.
	t.Setenv("BROKER_SIGNING_KEY", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	body := strings.Replace(validConfig,
		`publicURL: "https://broker.example.com"`,
		"publicURL: \"https://broker.example.com\"\n  mcp:\n    addr: \":8081\"\n    tls:\n      certFile: \"/etc/tls/cert.pem\"",
		1)
	_, err := LoadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatal("LoadConfig accepted MCP block with TLS cert-without-key — validate hook not wired into outer chain")
	}
	if !strings.Contains(err.Error(), "certFile and server.mcp.tls.keyFile must be set together") {
		t.Errorf("err = %v, want paired-TLS-fields message", err)
	}
}

func TestLoadConfigRejectsMatchingMgmtAndMCPAddrs(t *testing.T) {
	// Same listener for management API and MCP gateway would fail at
	// the second http.Server's Listen with EADDRINUSE — catch the
	// typo at config load so the operator sees a clear message
	// instead of a noisy bind error in logs.
	t.Setenv("BROKER_SIGNING_KEY", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	body := strings.Replace(validConfig,
		`publicURL: "https://broker.example.com"`,
		"publicURL: \"https://broker.example.com\"\n  mcp:\n    addr: \":8080\"",
		1)
	_, err := LoadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatal("LoadConfig accepted matching server.addr and server.mcp.addr — config-time collision check missing")
	}
	if !strings.Contains(err.Error(), "server.mcp.addr must differ from server.addr") {
		t.Errorf("err = %v, want addr-collision message", err)
	}
}

func TestLoadConfigMCPDefaultsAppliedWhenBlockOmitted(t *testing.T) {
	// Existing universe-onboarding configs have no `mcp:` block —
	// they predate the gateway plan. Upgrading to a broker with
	// always-on MCP must default Addr so those configs load without
	// modification.
	t.Setenv("BROKER_SIGNING_KEY", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	cfg, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.MCP.Addr != defaultMCPAddr {
		t.Errorf("MCP.Addr = %q, want default %q so existing configs upgrade silently", cfg.Server.MCP.Addr, defaultMCPAddr)
	}
}
