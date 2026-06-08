package broker

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the broker's YAML configuration. One file describes the broker's
// own runtime knobs, the OIDC provider it trusts, and the set of worlds it
// is authorized to mint tokens for. The world Tokens Secrets live in each
// world's namespace; the broker's own refresh-token and per-world
// write-token Secrets live in the broker's namespace.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	OIDC        OIDCConfig        `yaml:"oidc"`
	Worlds      []WorldConfig     `yaml:"worlds"`
	Sweeper     SweeperConfig     `yaml:"sweeper"`
	RateLimit   RateLimitConfig   `yaml:"rateLimit"`
	WorldDialer WorldDialerConfig `yaml:"worldDialer"`
}

// WorldDialerConfig holds knobs governing how the broker's MCP gateway
// dials world servers over QUIC. Single block at the top level (rather
// than per-world WorldConfig) because the dialer's posture is uniform
// across worlds — the trust model is "all worlds the broker is
// configured for are equally trusted." Per-world overrides would
// invite a misconfiguration where one world's relaxed posture leaks
// onto another via shared connection pooling.
type WorldDialerConfig struct {
	// InsecureSkipVerify disables the QUIC client's TLS chain +
	// hostname verification when dialing worlds. Required today for
	// in-cluster deployments where each world has a self-signed cert
	// from cert-manager's `selfsigned` ClusterIssuer (no shared CA the
	// broker can trust) — QUIC still encrypts on the wire, but the
	// broker takes on faith that the address it dialed actually
	// belongs to the world it expects. NetworkPolicy + cluster RBAC
	// are doing the real isolation work in that topology.
	//
	// Production-correct value once a shared CA is wired (e.g.,
	// cert-manager CA Issuer signing all world certs + a `caBundle`
	// config that loads it into the broker's RootCAs): false. The
	// flag stays as the operator escape hatch for environments where
	// shared CA setup isn't feasible.
	//
	// Default false. Operators who want skip-verify must opt in
	// explicitly so the verification posture is never silently relaxed.
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}

// ServerConfig holds the broker's own runtime settings — listen address,
// cookie-signing key, state-cookie TTL, and the broker namespace + Secret
// names where its refresh-token and per-world write-token state live.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":8080". HTTPS termination is the
	// Ingress's job; the broker speaks plain HTTP inside the cluster.
	Addr string `yaml:"addr"`
	// CookieKey is the base64-encoded HMAC key used to sign the OIDC state
	// cookie. Rotate by reissuing the Secret and bouncing the broker;
	// any in-flight login is invalidated, which is the desired behavior
	// (treat as if the user re-initiated the login).
	CookieKey string `yaml:"cookieKey"`
	// StateTTL caps the lifetime of the signed state cookie. 5m is the
	// default; long enough for a real user to complete the OIDC redirect,
	// short enough that a stale cookie cannot be replayed.
	StateTTL time.Duration `yaml:"stateTTL"`
	// BrokerNamespace is the k8s namespace the broker itself runs in;
	// the refresh-tokens and per-world write-token Secrets live here.
	BrokerNamespace string `yaml:"brokerNamespace"`
	// PublicURL is the broker's externally-reachable base URL (e.g.
	// https://broker.acmecorp.com), trailing slash stripped at load. Used as
	// the issuer + base for device_authorization_endpoint and token_endpoint
	// in /.well-known/openid-configuration (universe onboarding PR2). The
	// broker has no other way to know its own externally-visible URL —
	// OIDC.RedirectURL is purpose-specific (the IdP redirect target) and
	// may differ from the broker's base when the broker is fronted at a
	// path or behind an Ingress with rewrites.
	PublicURL string `yaml:"publicURL"`
	// InsecureCookies drops the Secure attribute on the OIDC state cookie.
	// Default false (production-correct: state cookies travel over HTTPS
	// only). Flip to true ONLY for kind / local dev where the broker is
	// reachable over plain HTTP and the Secure attribute would prevent the
	// cookie jar from replaying it on /auth/callback. Enabling this in a
	// real deployment turns the state cookie into a downgrade attack vector
	// — any plaintext-hijacked redirect can complete the OIDC dance.
	InsecureCookies bool `yaml:"insecureCookies"`
	// DeviceCodeTTL caps the lifetime of an RFC 8628 device-flow grant —
	// the window between /device/authorize and the user completing the
	// IdP redirect at /device. Default 10 minutes; matches the value
	// Google + Auth0 use, long enough for a user to switch devices and
	// complete the OIDC dance, short enough that an abandoned grant
	// evicts before clogging the in-memory store.
	DeviceCodeTTL time.Duration `yaml:"deviceCodeTTL"`
	// DevicePollInterval is the minimum allowed gap between successive
	// /device/token polls before the broker returns RFC 8628 slow_down.
	// Default 5s; matches the `interval` value the authorize response
	// emits to clients.
	DevicePollInterval time.Duration `yaml:"devicePollInterval"`
	// RefreshTokensSecret is the broker-namespace Secret holding the
	// JSON map of sha256(refresh_token) → record. Read+patched on every
	// /device/token refresh-grant call and /token/revoke. Required for
	// universe-onboarding PR4; defaulted in validate when omitted so
	// existing dev configs upgrade without an explicit value.
	RefreshTokensSecret string `yaml:"refreshTokensSecret"`
	// RefreshTokenTTL is the lifetime of a newly-issued refresh token.
	// Default 90 days. Tokens past their TTL are rejected at refresh
	// time and reaped by the periodic Sweeper. Operators tighten this
	// for short-lived deployments or relax it for headless CI tooling
	// where a 90-day re-login is acceptable.
	RefreshTokenTTL time.Duration `yaml:"refreshTokenTTL"`
	// IDTokenTTL is the lifetime applied to broker-signed id_tokens
	// emitted on the /device/token refresh-grant path. Default
	// 15 minutes — short enough that a compromised id_token's
	// useful window stays bounded, long enough that downstream
	// requests (PR5 /me/install) complete without racing the
	// expiry. Operators tighten via config; the refresh-grant
	// itself stretches the effective session lifetime via fresh
	// id_tokens minted on each poll.
	IDTokenTTL time.Duration `yaml:"idTokenTTL"`
	// MCP is the broker's MCP-gateway-specific config. The gateway
	// is opt-in: an operator runs the broker without MCP by leaving
	// `mcp.enabled` unset, in which case the existing management
	// API stays the only listener. When enabled, the MCP listener
	// binds its own Addr (separate from Server.Addr) and shares the
	// auth + rate-limit machinery with the rest of Server.
	// See plan: Broker MCP Gateway (Knowledge-System Layer).
	MCP MCPConfig `yaml:"mcp"`
}

// MCPConfig governs the broker's MCP-over-HTTPS gateway listener
// (knowledge-system layer). The gateway is always on — every broker
// is a knowledge-system gateway, the OAuth-only deployment shape is
// not a product we ship. Operators tune Addr and TLS here; the
// listener always binds.
//
// All 14 tools are live. Reads dispatch with an empty bearer (open to
// any SSO-authed identity); writes use a long-lived per-world token the
// broker provisions on first write. The FirstMint* knobs govern the
// retry loop that absorbs kubelet Secret-propagation lag on that first
// write — see the field docs.
type MCPConfig struct {
	// Addr is the MCP gateway's listen address, e.g. ":8081".
	// Defaulted to defaultMCPAddr when unset so existing deployments
	// pick up the gateway on upgrade without a values.yaml change.
	// Must differ from Server.Addr so the management API and the
	// MCP surface bind their own listeners — the chart routes the
	// two through distinct Ingress hosts or paths.
	Addr string `yaml:"addr"`
	// TLS terminates HTTPS at the broker when CertFile and KeyFile
	// are set. Leave blank to run plain HTTP inside the cluster (the
	// Ingress terminates HTTPS at the edge), matching the existing
	// Server.Addr convention. When set, both files must point at
	// readable PEM blobs.
	TLS MCPTLSConfig `yaml:"tls"`
	// FirstMintMaxAttempts caps the broker-side retry loop that
	// absorbs the kubelet → world tokens-Secret propagation lag
	// after a lazy mint: the world's projected Secret volume
	// typically refreshes within seconds, but worst-case can take
	// up to the kubelet sync period. The retry loop runs only when
	// the dispatch produces an `unauthorized` response on a token
	// the broker just minted (or just re-minted after a cache-hit
	// 401). Default 6. Subsequent attempts back off exponentially
	// from FirstMintInitialBackoff up to FirstMintMaxBackoff.
	FirstMintMaxAttempts int `yaml:"firstMintMaxAttempts"`
	// FirstMintInitialBackoff is the delay before the second
	// dispatch attempt. Doubles each iteration up to
	// FirstMintMaxBackoff. Default 250ms.
	FirstMintInitialBackoff time.Duration `yaml:"firstMintInitialBackoff"`
	// FirstMintMaxBackoff caps the exponential growth. Default 8s.
	// 250ms → 500ms → 1s → 2s → 4s → 8s sums to ~16s of waiting
	// across 6 attempts, well under the typical 60s worst-case
	// propagation window.
	FirstMintMaxBackoff time.Duration `yaml:"firstMintMaxBackoff"`
}

// defaultMCPAddr is the port the MCP gateway binds when MCPConfig.Addr
// is omitted. Picked to be distinct from the management API's typical
// :8080 so the two listeners don't collide in default deployments. The
// chart still surfaces an override.
const defaultMCPAddr = ":8081"

// MCPTLSConfig is the optional cert+key pair for the MCP gateway
// listener. Both fields must be set together; specifying one without
// the other is a config typo and fails validation.
type MCPTLSConfig struct {
	// CertFile is the path to the PEM-encoded certificate chain.
	CertFile string `yaml:"certFile"`
	// KeyFile is the path to the matching PEM-encoded private key.
	KeyFile string `yaml:"keyFile"`
}

// OIDCConfig describes the OIDC client registration at the IdP. Discovery
// is performed eagerly at broker startup against Issuer.
type OIDCConfig struct {
	// Issuer is the OIDC discovery URL, e.g. https://accounts.google.com.
	Issuer string `yaml:"issuer"`
	// ClientID and ClientSecret are the OAuth client credentials the
	// broker was registered with at the IdP.
	ClientID     string `yaml:"clientID"`
	ClientSecret string `yaml:"clientSecret"`
	// RedirectURL is the public URL of /auth/callback. Must exactly match
	// the redirect URI registered with the IdP.
	RedirectURL string `yaml:"redirectURL"`
	// BrokerSigningKey is the PEM-encoded ECDSA P-256 private key
	// the broker uses to sign id_tokens on the /device/token
	// refresh-grant path (PR4). PKCS#8 (`-----BEGIN PRIVATE KEY-----`)
	// or SEC1 (`-----BEGIN EC PRIVATE KEY-----`) formats accepted.
	// Required when any refresh-grant request might be issued —
	// production wiring requires it always; tests construct
	// Config{} directly and let NewServer skip key-dependent routes
	// when blank. Operators supply via Kubernetes Secret mounted as
	// a file; the chart renders the field's value from a
	// secretKeyRef so the key never round-trips through helm
	// release history.
	BrokerSigningKey string `yaml:"brokerSigningKey"`
	// AllowDomains is the broker-global Google Workspace hosted-domain
	// allowlist. When non-empty, every IdP exchange (device flow,
	// auth-code flow, and the legacy browser /auth/callback) is rejected
	// unless the verified id_token's `hd` claim matches one of these
	// entries. The check runs before any authorization code, refresh
	// token, or world dispatch is issued, so disallowed identities
	// never get past initial login. Empty disables the gate (any
	// verified identity passes the broker layer; per-world Allow lists
	// still apply downstream). Entries are lowercased+trimmed at
	// config load. Keying on `hd` rather than parsing the email domain
	// is the spoof-resistant choice: `hd` is set by Google from the
	// Workspace tenant and the user cannot influence it. Consumer
	// Google accounts (no `hd`) are rejected when this list is set.
	AllowDomains []string `yaml:"allowDomains"`
}

// WorldConfig describes one demarkus world the broker is authorized to
// mint tokens for. Authorization is per-world; an OIDC identity may
// qualify for some worlds and not others depending on Allow.
type WorldConfig struct {
	// Name is the world's logical name; the MCP gateway keys tool
	// calls and the per-world write-token store on it.
	Name string `yaml:"name"`
	// Namespace is the k8s namespace where this world's TokensSecret lives.
	Namespace string `yaml:"namespace"`
	// TokensSecret is the name of the world's tokens.toml Secret. The
	// broker requires get/patch on this Secret in that namespace.
	TokensSecret string `yaml:"tokensSecret"`
	// PublicURL is the world's reachable address for client installation
	// (typically `mark://host:port`). Consumed by /me/install (universe
	// onboarding) so the plugin can wire a client at this address. Optional:
	// when blank, the world is omitted from /me/install responses because
	// there is no address to hand the client. The field exists here on
	// WorldConfig — not in the world's own Secret — because the broker is
	// the single source of truth for what a user is told to connect to;
	// the world server itself does not know its externally-visible URL.
	PublicURL string `yaml:"publicURL"`
	// InternalAddress overrides the default `<name>.<namespace>.svc.
	// cluster.local:6309` resolution the MCP gateway uses to route
	// tool calls keyed by worldName. Optional: when blank, the gateway
	// builds the address from this world's Name + Namespace using the
	// standard Kubernetes Service-DNS convention. Operators set this
	// when they deploy a world with a Service name that diverges from
	// the world's logical Name (the demarkus-server chart's default
	// keeps them aligned, so most deployments leave this blank).
	// The Mark Protocol scheme is always `mark://`; the value here is
	// host:port only (e.g. `team-a-mark.team-a:6309`).
	InternalAddress string `yaml:"internalAddress"`
	// Allow is the per-world authorization predicate. See AllowConfig for
	// the rules. When every list is empty the world accepts any verified
	// identity (back-compat with pre-Slice-C configs that had only
	// AllowDomains and left it empty).
	Allow AllowConfig `yaml:"allow"`
	// DefaultToken carries the path scope applied to the broker's
	// long-lived write token for this world.
	DefaultToken TokenScope `yaml:"defaultToken"`
}

// AllowConfig describes who qualifies for tokens against a given world.
// Predicate (Slice C.1):
//
//  1. If Domains, Groups, and Emails are all empty, any verified identity
//     qualifies. This preserves the pre-Slice-C "no allowlist = open
//     world" behavior so existing configs upgrade without surprise.
//  2. Otherwise the identity qualifies if its email is in Emails (the
//     per-user carve-out), OR if its email matches Domains and its
//     groups intersect Groups. Either Domains or Groups may be empty
//     individually; an empty list in that dimension means "no
//     restriction on that dimension." But at least one of {Domains,
//     Groups, Emails} must match for the world to authorize.
//
// Emails exists so worlds can grant access to specific users when the
// IdP doesn't surface usable groups in the ID token (Google Workspace,
// some Entra configs). Groups exists so RBAC can live in the IdP
// instead of the broker config.
type AllowConfig struct {
	// Domains is the email-domain allowlist. A login's id_token.email
	// must end with one of these (case-insensitive) for the
	// domain-and-groups predicate to match. Empty disables this
	// dimension only — see the AllowConfig doc for the full rule.
	Domains []string `yaml:"domains"`
	// Groups is the OIDC `groups`-claim allowlist. The identity must
	// have at least one group in this list for the domain-and-groups
	// predicate to match. Match is case-insensitive: entries are
	// lowercased+trimmed at config load, and groupsMatch lowercases the
	// claim's groups for compare. Our validated IdP set (Google, Okta,
	// Entra ID, Auth0) treats group names case-insensitively; case-
	// sensitive providers like Keycloak with deliberately distinct
	// case-variant groups are out of scope. IdP-specific: Okta and
	// Auth0 emit `groups` in the ID token when configured; Entra needs
	// the optional `groups` claim enabled; Google does not surface
	// groups in the ID token (use Emails as a carve-out, or wait for a
	// userinfo-based Verifier — backlogged).
	Groups []string `yaml:"groups"`
	// Emails is the per-user carve-out. A login whose verified email
	// matches an entry here qualifies regardless of Domains and Groups.
	// Entries are lowercased + trimmed at config load so the runtime
	// compare is case-insensitive without per-call normalization.
	Emails []string `yaml:"emails"`
}

// SweeperConfig knobs control the broker's periodic refresh-token
// expiry janitor (Slice C.2). The sweeper runs by default in every
// broker; multi-replica deployments need it so expired refresh tokens
// age out of the broker-namespace Secret. Leader election timings are
// not yet exposed — client-go defaults (15s lease, 10s renew, 2s retry)
// suffice for our failover budget and revisit only if a customer needs
// faster failover.
type SweeperConfig struct {
	// Disabled is the opt-out switch. Omitting the block in YAML
	// (zero-value false) gives the production-correct behavior: the
	// sweeper runs. Set to true only in dev configs where the operator
	// wants expired tokens to linger for inspection, or in a
	// single-replica deployment that wants to skip the leader-election
	// overhead. Named "Disabled" rather than "Enabled" so the
	// zero-value default is the safe one.
	Disabled bool `yaml:"disabled"`
	// Interval is the time between sweep passes. Default 5m. Shorter
	// values shrink the worst-case window between token expiry and the
	// world Secret reflecting it, at the cost of more k8s API churn.
	Interval time.Duration `yaml:"interval"`
	// LeaseName is the coordination.k8s.io/Lease object the broker
	// replicas race for, in Server.BrokerNamespace. Default
	// "demarkus-broker-sweeper". Override only when running multiple
	// independent broker deployments in the same namespace (rare).
	LeaseName string `yaml:"leaseName"`
}

// RateLimitConfig caps per-subject and per-IP request rates against the
// broker's HTTP surface (Slice C.4). Per-replica only: each broker pod
// keeps its own in-memory limiter, so the effective rate seen by an
// abusive client is up to N× the configured value in a multi-replica
// deployment. Documented as a §Risks bullet; cluster-shared rate
// limiting is a Phase 7+ follow-up when a customer's scale needs it.
//
// Defaults come from plan §6.2 Slice C.4: 10/min subject on
// /me/install (burst 5); 20/min IP on /auth/login (burst 5). Operators
// tighten or loosen via YAML.
type RateLimitConfig struct {
	// Disabled is the opt-out switch, same naming convention as
	// SweeperConfig.Disabled. Zero-value (false) means the limiter
	// runs with the configured rates, so an operator who omits the
	// rateLimit block in their values file still gets the protection.
	Disabled bool `yaml:"disabled"`
	// Tokens governs the per-subject bucket fronting GET /me/install,
	// the only subject-rate-limited route. (The yaml key is kept as
	// `tokens` for config back-compat with deployments authored before
	// the per-user /tokens routes were removed.)
	Tokens RateLimitRouteConfig `yaml:"tokens"`
	// Login governs the per-IP bucket on GET /auth/login. The
	// callback is intentionally not rate-limited at this layer: the
	// signed state cookie (HMAC-SHA256, 5-minute TTL) is the natural
	// rate gate on /auth/callback, and adding an IP bucket there
	// would penalize legitimate redirects from heavily-NATted
	// corporate networks where many users share an egress IP.
	Login RateLimitRouteConfig `yaml:"login"`
	// TrustForwardedFor opts the IP limiter into honoring the
	// leftmost X-Forwarded-For entry as the client IP. Default false
	// because trusting XFF when the broker is not actually behind a
	// proxy lets an attacker spoof the header to bypass per-IP
	// limits. Operators flip this to true once the broker is
	// deployed behind an Ingress/LB that strips spoofed XFF and
	// appends the real client IP.
	TrustForwardedFor bool `yaml:"trustForwardedFor"`
}

// RateLimitRouteConfig is the per-bucket knobs for a route group. Both
// fields are required when the parent block is present and Disabled is
// false; validation surfaces missing values at load rather than letting
// a zero-rate limiter reject every request at runtime.
type RateLimitRouteConfig struct {
	// PerMinute is the steady-state replenishment rate in events per
	// minute. We expose minutes (not events/sec) so the operator-
	// facing knob matches the human-scale numbers in the plan.
	PerMinute int `yaml:"perMinute"`
	// Burst is the size of the token bucket, i.e. the largest sudden
	// spike accepted before throttling kicks in. Must be >= 1; a
	// zero burst with positive PerMinute would never accept any
	// request because the limiter starts with an empty bucket.
	Burst int `yaml:"burst"`
}

// TokenScope is the path scope of the broker's long-lived write token
// for a world. The operations are always ["publish"] (hardcoded in
// worldWriteTokenStore.Provision so an open-reads regression can't slip
// in via config) and the token never expires, so the only operator
// knob is the path glob list written into the world's tokens.toml.
type TokenScope struct {
	// Paths is the list of glob patterns the write token is allowed to
	// access, e.g. ["/team-a/*"]. Empty means no path restriction
	// (server-side default: deny). Validation: at least one entry is
	// required to avoid accidentally issuing a zero-scope token.
	Paths []string `yaml:"paths"`
}

// LoadConfig reads, parses, and validates a broker config file. Validation
// errors are surfaced eagerly so a misconfigured broker fails to start
// rather than half-working until the first OIDC callback.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("broker: read config %s: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("broker: parse config %s: %w", path, err)
	}
	c.applyEnvOverrides()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("broker: validate config %s: %w", path, err)
	}
	return &c, nil
}

// applyEnvOverrides lets a small set of high-sensitivity fields come from
// environment variables instead of the on-disk config. Supported:
//
//   - OIDC_CLIENT_SECRET: the OAuth client secret.
//   - BROKER_SIGNING_KEY: the ECDSA P-256 PEM the broker uses to
//     sign id_tokens on the refresh-grant path (PR4). Multi-line
//     PEMs travel fine through env vars under k8s secretKeyRef.
//
// Production deployments keep these in externally-managed
// Kubernetes Secrets (External Secrets Operator, Sealed Secrets,
// Vault) mounted via secretKeyRef rather than baked into the chart-
// rendered config Secret where they would leak into helm release
// history. The env var wins over the file value when both are set;
// an empty env var is treated as unset so an accidentally cleared
// variable doesn't blank out a file-supplied value at runtime.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("OIDC_CLIENT_SECRET"); v != "" {
		c.OIDC.ClientSecret = v
	}
	if v := os.Getenv("BROKER_SIGNING_KEY"); v != "" {
		c.OIDC.BrokerSigningKey = v
	}
}

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	if c.Server.CookieKey == "" {
		return fmt.Errorf("server.cookieKey is required")
	}
	if c.Server.BrokerNamespace == "" {
		return fmt.Errorf("server.brokerNamespace is required")
	}
	// Normalize before the empty-check so values like "   " or "/" are
	// caught here instead of silently producing a broken issuer URL
	// downstream (e.g. "/device/authorize" with no scheme/host).
	c.Server.PublicURL = strings.TrimRight(strings.TrimSpace(c.Server.PublicURL), "/")
	if c.Server.PublicURL == "" {
		return fmt.Errorf("server.publicURL is required")
	}
	// Enforce absolute-URL shape. Without scheme+host the override would
	// emit values like "/device/authorize" into the discovery doc, which
	// every OIDC client would reject — fail fast at config load instead
	// of producing a broker that boots but serves a useless well-known.
	if u, err := url.Parse(c.Server.PublicURL); err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("server.publicURL must be an absolute URL (got %q)", c.Server.PublicURL)
	}
	if c.Server.StateTTL == 0 {
		c.Server.StateTTL = 5 * time.Minute
	}
	if c.Server.StateTTL < 0 {
		return fmt.Errorf("server.stateTTL must be > 0 (got %s)", c.Server.StateTTL)
	}
	if err := c.Server.applyDeviceFlowDefaults(); err != nil {
		return err
	}
	if err := c.Server.applyRefreshDefaults(); err != nil {
		return err
	}
	if err := c.Server.MCP.validate(); err != nil {
		return err
	}
	// The management API and the MCP gateway must bind distinct
	// addresses — same listener for both would fail at the second
	// http.Server's Listen with a "address already in use" error at
	// startup. Catching it at config-load surfaces the typo with a
	// clear message instead of a noisy bind error in the broker's
	// log stream.
	if c.Server.MCP.Addr == c.Server.Addr {
		return fmt.Errorf("server.mcp.addr must differ from server.addr (both are %q)", c.Server.Addr)
	}
	if err := c.OIDC.validate(); err != nil {
		return err
	}
	if len(c.Worlds) == 0 {
		return fmt.Errorf("at least one world is required")
	}
	seen := make(map[string]bool, len(c.Worlds))
	for i := range c.Worlds {
		w := &c.Worlds[i]
		if err := validateWorld(i, w); err != nil {
			return err
		}
		if seen[w.Name] {
			return fmt.Errorf("worlds[%d]: duplicate name %q", i, w.Name)
		}
		seen[w.Name] = true
	}
	// Sweeper defaults. Disabled is the zero-value opt-out — see the
	// SweeperConfig doc for why it's named "Disabled" rather than
	// "Enabled". Interval and LeaseName get production-safe defaults
	// when omitted; negative Interval is a config typo, and an
	// over-long interval lets expired refresh tokens linger in the
	// broker's refresh-tokens Secret well past their TTL before the
	// sweeper retires them.
	if c.Sweeper.Interval == 0 {
		c.Sweeper.Interval = 5 * time.Minute
	}
	if c.Sweeper.Interval < 0 {
		return fmt.Errorf("sweeper.interval must be > 0 (got %s)", c.Sweeper.Interval)
	}
	if c.Sweeper.Interval > maxSweeperInterval {
		return fmt.Errorf("sweeper.interval must be <= %s (got %s); intervals longer than a token lifetime defeat expiry-driven revocation", maxSweeperInterval, c.Sweeper.Interval)
	}
	if c.Sweeper.LeaseName == "" {
		c.Sweeper.LeaseName = "demarkus-broker-sweeper"
	}
	return c.RateLimit.applyDefaultsAndValidate()
}

// validate enforces the OIDCConfig invariants. Extracted from
// Config.validate so the outer function stays inside the gocyclo
// budget after PR4 added BrokerSigningKey alongside the existing
// four required fields. All five are operator-supplied and missing
// any one of them makes the broker incapable of completing a login
// or signing a refresh-grant response.
//
// BrokerSigningKey is parsed (not just checked for emptiness)
// because a malformed PEM would otherwise survive LoadConfig and
// only surface in main.run after OIDC discovery + kube client setup
// have already burned several seconds and produced noisier logs.
// Parsing here costs a microsecond and lets the operator-facing
// error reference oidc.brokerSigningKey directly.
func (o *OIDCConfig) validate() error {
	switch {
	case o.Issuer == "":
		return fmt.Errorf("oidc.issuer is required")
	case o.ClientID == "":
		return fmt.Errorf("oidc.clientID is required")
	case o.ClientSecret == "":
		return fmt.Errorf("oidc.clientSecret is required")
	case o.RedirectURL == "":
		return fmt.Errorf("oidc.redirectURL is required")
	case o.BrokerSigningKey == "":
		return fmt.Errorf("oidc.brokerSigningKey is required")
	}
	if _, err := NewIDTokenSigner([]byte(o.BrokerSigningKey)); err != nil {
		return fmt.Errorf("oidc.brokerSigningKey is invalid: %w", err)
	}
	for i, d := range o.AllowDomains {
		norm := strings.ToLower(strings.TrimSpace(d))
		if norm == "" {
			return fmt.Errorf("oidc.allowDomains[%d] is empty", i)
		}
		o.AllowDomains[i] = norm
	}
	return nil
}

// applyRefreshDefaults fills in PR4 refresh-flow defaults and rejects
// degenerate combinations. Extracted from Config.validate to keep
// the outer function inside the gocyclo budget; same shape as
// applyDeviceFlowDefaults. The Secret name and TTL knobs are
// applied to the validated config so NewServer / LoadConfig
// downstream see the resolved values regardless of YAML presence.
func (s *ServerConfig) applyRefreshDefaults() error {
	if s.RefreshTokensSecret == "" {
		s.RefreshTokensSecret = defaultRefreshTokensSecret
	}
	if s.RefreshTokenTTL == 0 {
		s.RefreshTokenTTL = defaultRefreshTokenTTL
	}
	if s.RefreshTokenTTL < 0 {
		return fmt.Errorf("server.refreshTokenTTL must be > 0 (got %s)", s.RefreshTokenTTL)
	}
	if s.IDTokenTTL == 0 {
		s.IDTokenTTL = defaultIDTokenTTL
	}
	if s.IDTokenTTL < 0 {
		return fmt.Errorf("server.idTokenTTL must be > 0 (got %s)", s.IDTokenTTL)
	}
	// id_token TTL > refresh_token TTL is a degenerate config: the
	// refresh credential would expire before the bearer it mints,
	// so the refresh round-trip would never produce a usable token.
	if s.IDTokenTTL >= s.RefreshTokenTTL {
		return fmt.Errorf("server.idTokenTTL (%s) must be < server.refreshTokenTTL (%s) — a bearer that outlives its refresh credential cannot be renewed", s.IDTokenTTL, s.RefreshTokenTTL)
	}
	return nil
}

// applyDeviceFlowDefaults fills in PR3 device-flow defaults and rejects
// degenerate combinations. Extracted from Config.validate to keep the
// outer function inside the gocyclo budget. PollInterval must be
// strictly less than the TTL — a configuration where every legitimate
// poll trips slow_down would deadlock a real client.
func (s *ServerConfig) applyDeviceFlowDefaults() error {
	if s.DeviceCodeTTL == 0 {
		s.DeviceCodeTTL = 10 * time.Minute
	}
	if s.DeviceCodeTTL < 0 {
		return fmt.Errorf("server.deviceCodeTTL must be > 0 (got %s)", s.DeviceCodeTTL)
	}
	if s.DevicePollInterval == 0 {
		s.DevicePollInterval = 5 * time.Second
	}
	if s.DevicePollInterval < 0 {
		return fmt.Errorf("server.devicePollInterval must be > 0 (got %s)", s.DevicePollInterval)
	}
	if s.DevicePollInterval >= s.DeviceCodeTTL {
		return fmt.Errorf("server.devicePollInterval (%s) must be < server.deviceCodeTTL (%s)", s.DevicePollInterval, s.DeviceCodeTTL)
	}
	return nil
}

// validate enforces the MCPConfig invariants. Addr is defaulted to
// defaultMCPAddr when omitted so an upgrade picks up the gateway
// without a values.yaml change. The TLS fields are paired: supplying
// one without the other is a config typo that would either fail at
// ListenAndServeTLS startup with a noisy error, or silently downgrade
// to plain HTTP. Fail fast at LoadConfig so the operator sees the
// misconfiguration before the broker pod starts.
//
// Defaults: FirstMintMaxAttempts=6, FirstMintInitialBackoff=250ms,
// FirstMintMaxBackoff=8s. The retry knobs absorb the kubelet → world
// Secret propagation lag after the first write provisions a world's
// token. Negative values are config typos and rejected.
func (m *MCPConfig) validate() error {
	if m.Addr == "" {
		m.Addr = defaultMCPAddr
	}
	hasCert := m.TLS.CertFile != ""
	hasKey := m.TLS.KeyFile != ""
	if hasCert != hasKey {
		return fmt.Errorf("server.mcp.tls.certFile and server.mcp.tls.keyFile must be set together")
	}
	if m.FirstMintMaxAttempts == 0 {
		m.FirstMintMaxAttempts = defaultFirstMintMaxAttempts
	}
	if m.FirstMintMaxAttempts < 0 {
		return fmt.Errorf("server.mcp.firstMintMaxAttempts must be > 0 (got %d)", m.FirstMintMaxAttempts)
	}
	if m.FirstMintInitialBackoff == 0 {
		m.FirstMintInitialBackoff = defaultFirstMintInitialBackoff
	}
	if m.FirstMintInitialBackoff < 0 {
		return fmt.Errorf("server.mcp.firstMintInitialBackoff must be > 0 (got %s)", m.FirstMintInitialBackoff)
	}
	if m.FirstMintMaxBackoff == 0 {
		m.FirstMintMaxBackoff = defaultFirstMintMaxBackoff
	}
	if m.FirstMintMaxBackoff < 0 {
		return fmt.Errorf("server.mcp.firstMintMaxBackoff must be > 0 (got %s)", m.FirstMintMaxBackoff)
	}
	if m.FirstMintInitialBackoff > m.FirstMintMaxBackoff {
		return fmt.Errorf("server.mcp.firstMintInitialBackoff (%s) must be <= firstMintMaxBackoff (%s)", m.FirstMintInitialBackoff, m.FirstMintMaxBackoff)
	}
	return nil
}

// Defaults for the MCP gateway's first-mint retry loop. Picked to
// absorb the typical kubelet Secret-propagation window without holding
// a tool call open longer than a careful user would wait. Operators
// tune in values.yaml.
const (
	defaultFirstMintMaxAttempts    = 6
	defaultFirstMintInitialBackoff = 250 * time.Millisecond
	defaultFirstMintMaxBackoff     = 8 * time.Second
)

// applyDefaultsAndValidate fills in plan §6.2 Slice C.4 defaults for the
// rate-limit knobs and rejects negative values. Per-field defaults are
// applied only when Disabled is false, so a fully-opted-out config can
// leave the per-route blocks empty without tripping validation. Extracted
// from validate() to keep both functions inside the gocyclo budget.
func (r *RateLimitConfig) applyDefaultsAndValidate() error {
	if r.Disabled {
		return nil
	}
	if r.Tokens.PerMinute == 0 {
		r.Tokens.PerMinute = 10
	}
	if r.Tokens.Burst == 0 {
		r.Tokens.Burst = 5
	}
	if r.Login.PerMinute == 0 {
		r.Login.PerMinute = 20
	}
	if r.Login.Burst == 0 {
		r.Login.Burst = 5
	}
	switch {
	case r.Tokens.PerMinute < 0:
		return fmt.Errorf("rateLimit.tokens.perMinute must be >= 1 (got %d)", r.Tokens.PerMinute)
	case r.Tokens.Burst < 0:
		return fmt.Errorf("rateLimit.tokens.burst must be >= 1 (got %d)", r.Tokens.Burst)
	case r.Login.PerMinute < 0:
		return fmt.Errorf("rateLimit.login.perMinute must be >= 1 (got %d)", r.Login.PerMinute)
	case r.Login.Burst < 0:
		return fmt.Errorf("rateLimit.login.burst must be >= 1 (got %d)", r.Login.Burst)
	}
	return nil
}

// maxSweeperInterval caps Sweeper.Interval at 24h — the canonical
// short-lived-token lifetime in plan §Revocation. Intervals longer
// than this leave expired tokens accepted past their ExpiresAt for
// up to a whole sweep cycle, breaking the contract that callers
// expect "expires" to mean.
const maxSweeperInterval = 24 * time.Hour

// validateWorld enforces the per-world invariants and normalizes the
// AllowConfig string lists in place. Split out of validate() so the
// gocyclo budget on each function stays inside the linter threshold;
// the outer loop owns cross-world checks (duplicates), the helper owns
// single-world checks.
func validateWorld(i int, w *WorldConfig) error {
	switch {
	case w.Name == "":
		return fmt.Errorf("worlds[%d]: name is required", i)
	case w.Namespace == "":
		return fmt.Errorf("worlds[%d] (%s): namespace is required", i, w.Name)
	case w.TokensSecret == "":
		return fmt.Errorf("worlds[%d] (%s): tokensSecret is required", i, w.Name)
	case len(w.DefaultToken.Paths) == 0:
		return fmt.Errorf("worlds[%d] (%s): defaultToken.paths is required", i, w.Name)
	}
	// All three lists are lowercased+trimmed at load so authorizedWorlds
	// can do plain string compares on every login. Group-name match is
	// case-insensitive because our validated IdP set (Google, Okta,
	// Entra ID, Auth0) enforces case-insensitive group-name uniqueness;
	// an operator writing "Engineering" while the IdP emits
	// "engineering" after upstream normalization would otherwise fail
	// silently. Case-sensitive providers like Keycloak with
	// deliberately distinct case-variant groups are out of scope; revisit
	// if a customer asks. Empty entries are always config typos and
	// surface here at load time rather than silently never matching.
	if err := normalizeAllowList(i, w.Name, "domains", w.Allow.Domains); err != nil {
		return err
	}
	if err := normalizeAllowList(i, w.Name, "emails", w.Allow.Emails); err != nil {
		return err
	}
	return normalizeAllowList(i, w.Name, "groups", w.Allow.Groups)
}

func normalizeAllowList(worldIdx int, worldName, field string, list []string) error {
	for j, s := range list {
		norm := strings.ToLower(strings.TrimSpace(s))
		if norm == "" {
			return fmt.Errorf("worlds[%d] (%s): allow.%s[%d] is empty", worldIdx, worldName, field, j)
		}
		list[j] = norm
	}
	return nil
}
