package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// Config is the broker's YAML configuration: its own runtime knobs, the OIDC
// provider it trusts and the worlds of both gateway profiles. World Tokens
// Secrets live in each world's namespace; the broker's own state in its namespace.
type Config struct {
	Server       ServerConfig       `yaml:"server"`
	OIDC         OIDCConfig         `yaml:"oidc"`
	Worlds       []WorldConfig      `yaml:"worlds"`
	WebClients   []WebClientConfig  `yaml:"webClients"`
	Sweeper      SweeperConfig      `yaml:"sweeper"`
	RateLimit    RateLimitConfig    `yaml:"rateLimit"`
	WorldDialer  WorldDialerConfig  `yaml:"worldDialer"`
	Provisioning ProvisioningConfig `yaml:"provisioning"`
	AgentTokens  []AgentTokenConfig `yaml:"agentTokens"`

	// registry is the live world set, built from Worlds on first use.
	registryOnce sync.Once
	registry     *WorldRegistry
}

// Registry is the live world set of both profiles: Worlds as loaded, plus
// what provisioning adds. Each gateway reads its own View of it.
func (c *Config) Registry() *WorldRegistry {
	c.registryOnce.Do(func() { c.registry = newWorldRegistry(c.Worlds) })
	return c.registry
}

// Gateway profiles: the knowledge surface (org open reads, per world Allow
// writes) and the memory surface (identity = world).
const (
	ProfileKnowledge = "knowledge"
	ProfileMemory    = "memory"
)

// MCPPath is the JSON-RPC endpoint of every gateway; the RFC 8707 resource
// indicator and the RFC 9728 path-inserted metadata route derive from it.
const MCPPath = "/mcp"

// WebClientConfig registers one confidential web client (RFC 6749 2.1): a
// server side app with a secret and a real https redirect. The list is operator
// curated; /register stays public-client-only and never adds to it.
type WebClientConfig struct {
	// ClientID is presented on /oauth/authorize and the token endpoint.
	ClientID string `yaml:"clientID"`
	// ClientSecret is the cleartext secret, hashed into ClientSecretHash at
	// load and cleared. Set this, ClientSecretHash, or ClientSecretEnv.
	ClientSecret string `yaml:"clientSecret"`
	// ClientSecretEnv names an environment variable holding the cleartext
	// secret (a secretKeyRef), read at load into ClientSecret.
	ClientSecretEnv string `yaml:"clientSecretEnv"`
	// ClientSecretHash is the lowercase sha256 hex of the client secret. sha256
	// suffices because the secret is operator generated randomness, not a password.
	ClientSecretHash string `yaml:"clientSecretHash"`
	// RedirectURIs is the exact match allowlist: absolute https URLs, no
	// wildcards, no loopback exemption. Anything looser is an open redirect.
	RedirectURIs []string `yaml:"redirectURIs"`
	// Name is an optional human label for logs.
	Name string `yaml:"name"`
}

// HashClientSecret is the lowercase sha256 hex a web client secret is
// registered and compared as.
func HashClientSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// WebClient returns the registered confidential client for clientID, or false
// when the id is unregistered (the public/native path).
func (c *Config) WebClient(clientID string) (*WebClientConfig, bool) {
	for i := range c.WebClients {
		if c.WebClients[i].ClientID == clientID {
			return &c.WebClients[i], true
		}
	}
	return nil, false
}

// AllowsRedirect is a byte for byte compare per OAuth 2.1; normalization
// (case, default ports, dot segments) is how redirect allowlists get bypassed.
func (wc *WebClientConfig) AllowsRedirect(raw string) bool {
	return slices.Contains(wc.RedirectURIs, raw)
}

// WorldDialerConfig governs how the MCP gateway dials worlds over QUIC. One
// block for every world: a per world posture could leak across the shared pool.
type WorldDialerConfig struct {
	// InsecureSkipVerify skips TLS chain and hostname checks on dial. Needed
	// where worlds carry self signed certs with no shared CA; QUIC still
	// encrypts, but the broker trusts the address it dialed. Opt in only.
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}

// ServerConfig holds the broker's own runtime settings.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":8080". HTTPS terminates at the Ingress.
	Addr string `yaml:"addr"`
	// BearerAddr is the UDP address of the knowledge server's bearer
	// listener, mark requests with an identity bearer in the auth field
	// (ADR 0033). Blank opens none.
	BearerAddr string `yaml:"bearerAddr"`
	// CookieKey is the base64 HMAC key signing the OIDC state cookie. Rotating
	// it invalidates in-flight logins. Blank makes the broker generate one on
	// first start and persist it in CookieKeySecret.
	CookieKey string `yaml:"cookieKey"`
	// CookieKeySecret holds the generated state cookie key. Default
	// "demarkus-broker-cookie-key".
	CookieKeySecret string `yaml:"cookieKeySecret"`
	// StateTTL caps the signed state cookie. Default 5m.
	StateTTL time.Duration `yaml:"stateTTL"`
	// BrokerNamespace is where the broker's own Secrets live.
	BrokerNamespace string `yaml:"brokerNamespace"`
	// PublicURL is the broker's externally reachable base URL, the issuer of
	// its discovery document. OIDC.RedirectURL may differ behind a rewrite.
	PublicURL string `yaml:"publicURL"`
	// InsecureCookies drops the Secure attribute on the state cookie. For
	// plain HTTP dev only; in production it makes the cookie hijackable.
	InsecureCookies bool `yaml:"insecureCookies"`
	// DeviceCodeTTL caps an RFC 8628 grant. Default 10m, as Google and Auth0.
	DeviceCodeTTL time.Duration `yaml:"deviceCodeTTL"`
	// DevicePollInterval is the minimum gap between /device/token polls before
	// slow_down. Default 5s; also the interval advertised to clients.
	DevicePollInterval time.Duration `yaml:"devicePollInterval"`
	// RefreshTokensSecret holds the sha256(refresh_token) to record map.
	RefreshTokensSecret string `yaml:"refreshTokensSecret"`
	// SigningKeySecret holds the generated id_token signing key when
	// OIDC.BrokerSigningKey is blank. Default "demarkus-broker-signing-key".
	SigningKeySecret string `yaml:"signingKeySecret"`
	// DynamicClientsSecret holds the RFC 7591 registration map.
	DynamicClientsSecret string `yaml:"dynamicClientsSecret"`
	// RefreshTokenTTL is the lifetime of a new refresh token. Default 90 days.
	RefreshTokenTTL time.Duration `yaml:"refreshTokenTTL"`
	// IDTokenTTL is the lifetime of a broker signed id_token. Default 15m.
	IDTokenTTL time.Duration `yaml:"idTokenTTL"`
	// Realm names the product in the gateways' WWW-Authenticate challenge;
	// blank lets each profile name itself.
	Realm string `yaml:"realm"`
	// MCP is the knowledge gateway. It answers /mcp on every host the memory
	// gateway does not claim.
	MCP GatewayConfig `yaml:"mcp"`
	// Memory is the memory gateway, on when PublicURL is set; requests whose
	// Host is its hostname reach it.
	Memory GatewayConfig `yaml:"memory"`
}

// GatewayConfig is one gateway's public identity and tool surface.
type GatewayConfig struct {
	// PublicURL is the gateway's base URL, the hostname requests carry and
	// the RFC 9728 resource metadata. The knowledge gateway defaults it to
	// Server.PublicURL; the memory gateway is off without one.
	PublicURL string `yaml:"publicURL"`
	// ToolProfile selects the tool surface: "lean" omits operator and
	// federation tools; "full" (default) keeps every tool.
	ToolProfile string `yaml:"toolProfile"`
}

// Enabled reports whether the gateway is configured at all.
func (g GatewayConfig) Enabled() bool { return g.PublicURL != "" }

// Resource is the RFC 8707 resource indicator tokens for this gateway carry.
func (g GatewayConfig) Resource() string { return g.PublicURL + MCPPath }

// Host is the hostname the mux selects this gateway by, without a port.
func (g GatewayConfig) Host() string {
	u, err := url.Parse(g.PublicURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Gateway is the profile's gateway config. The knowledge gateway defaults
// its URL to the issuer here, the one place, so configs built without the
// loader resolve the same URL.
func (s *ServerConfig) Gateway(profile string) GatewayConfig {
	if profile == ProfileMemory {
		return s.Memory
	}
	gw := s.MCP
	if gw.PublicURL == "" {
		gw.PublicURL = s.PublicURL
	}
	return gw
}

// Resources are the RFC 8707 resource indicators this broker issues tokens
// for: one per configured gateway, canonical.
func (s *ServerConfig) Resources() []string {
	out := []string{s.Gateway(ProfileKnowledge).Resource()}
	if s.Memory.Enabled() {
		out = append(out, s.Memory.Resource())
	}
	return out
}

// CanonicalResource normalizes a resource indicator the way public URLs are
// normalized at load, so a client's spelling compares to Resources.
func CanonicalResource(raw string) (string, error) {
	resource, err := normalizePublicURL("resource", raw)
	if err == nil && resource == "" {
		return "", fmt.Errorf("resource must be an absolute URL")
	}
	return resource, err
}

// OIDCConfig describes the OIDC client registration at the IdP. Discovery
// runs eagerly at startup against Issuer.
type OIDCConfig struct {
	// Issuer is the OIDC discovery URL, e.g. https://accounts.google.com.
	Issuer string `yaml:"issuer"`
	// ClientID and ClientSecret are the broker's registered credentials.
	ClientID     string `yaml:"clientID"`
	ClientSecret string `yaml:"clientSecret"`
	// RedirectURL is the public URL of /auth/callback, as registered at the IdP.
	RedirectURL string `yaml:"redirectURL"`
	// BrokerSigningKey is the PEM ECDSA P-256 key (PKCS#8 or SEC1) signing
	// broker id_tokens. Blank makes the broker generate one on first start
	// and persist it in Server.SigningKeySecret.
	BrokerSigningKey string `yaml:"brokerSigningKey"`
	// AllowDomains is the Google Workspace hosted domain allowlist, checked on
	// every IdP exchange before any code or token is issued. Keyed on the `hd`
	// claim, which the user cannot influence; consumer accounts have none.
	AllowDomains []string `yaml:"allowDomains"`
}

// WorldConfig describes one world a gateway serves. Authorization is per
// world through Allow.
type WorldConfig struct {
	// Generation tells one provisioning of a name from the next, so state
	// cached for a deprovisioned world is not reused by its successor.
	Generation string `yaml:"-"`
	// Name keys tool calls and the write grant.
	Name string `yaml:"name"`
	// Profile is the gateway that serves the world, knowledge or memory.
	// Required on every world once the memory gateway is configured.
	Profile string `yaml:"profile"`
	// Namespace is where TokensSecret lives.
	Namespace string `yaml:"namespace"`
	// TokensSecret is the world's tokens.toml Secret; the broker needs get and patch.
	TokensSecret string `yaml:"tokensSecret"`
	// PublicURL is the address handed to clients by /me/install. The broker is
	// the one source for it; a world does not know its external URL. Optional:
	// blank omits the world from /me/install.
	PublicURL string `yaml:"publicURL"`
	// InternalAddress is host:port for tool calls; blank derives
	// `<name>.<namespace>.svc.cluster.local:6309`.
	InternalAddress string `yaml:"internalAddress"`
	// DialAddress splits the socket target from the logical authority: dial
	// here, keep SNI and URL identity on InternalAddress.
	DialAddress string `yaml:"dialAddress"`
	// Local marks a world the knowledge server in this process serves: tool
	// calls run in process and the server must route its authority at start.
	Local bool `yaml:"local"`
	// Allow is the per world authorization predicate; all lists empty admits
	// any verified identity.
	Allow AllowConfig `yaml:"allow"`
	// WriteScope is the path scope the gateway may write in this world.
	WriteScope WriteScope `yaml:"writeScope"`
}

// Address is the world's host:port: InternalAddress when set, else the
// Kubernetes Service DNS name on the protocol's default port.
func (w *WorldConfig) Address() string {
	if w.InternalAddress != "" {
		return w.InternalAddress
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", w.Name, w.Namespace, protocol.DefaultPort)
}

// Authority is the host of Address, the SNI a QUIC client presents and the
// key a server in this process routes a local world by.
func (w *WorldConfig) Authority() string {
	return fetch.AuthorityHostname(w.Address())
}

// AllowConfig is the per world predicate: all lists empty admits any verified
// identity; otherwise the email is in Emails, or it matches Domains and the
// groups intersect Groups, an empty dimension being unrestricted.
type AllowConfig struct {
	// Domains is the email domain allowlist, case insensitive.
	Domains []string `yaml:"domains"`
	// Groups is the `groups` claim allowlist, case insensitive: the validated
	// IdPs (Google, Okta, Entra, Auth0) treat group names that way.
	Groups []string `yaml:"groups"`
	// Emails is the per user carve out, for IdPs that surface no groups.
	Emails []string `yaml:"emails"`
}

// Empty reports an Allow with no lists, which admits every verified identity.
func (a *AllowConfig) Empty() bool {
	return len(a.Domains) == 0 && len(a.Groups) == 0 && len(a.Emails) == 0
}

// SweeperConfig tunes the refresh token expiry janitor. Leader election
// timings stay at client-go defaults until a deployment needs faster failover.
type SweeperConfig struct {
	// Disabled opts out; the zero value runs the sweeper, the safe default.
	Disabled bool `yaml:"disabled"`
	// Interval is the time between passes. Default 5m.
	Interval time.Duration `yaml:"interval"`
	// LeaseName is the Lease replicas race for, in Server.BrokerNamespace.
	LeaseName string `yaml:"leaseName"`
}

// RateLimitConfig caps per subject and per IP rates. Per replica only: a
// multi replica deployment sees up to N times the configured rate.
type RateLimitConfig struct {
	// Disabled opts out; the zero value limits with the defaults.
	Disabled bool `yaml:"disabled"`
	// Tokens is the per subject bucket on GET /me/install. The yaml key keeps
	// its old name for config compatibility.
	Tokens RateLimitRouteConfig `yaml:"tokens"`
	// Login is the per IP bucket on GET /auth/login. The callback is not
	// limited: its signed state cookie is the gate, and NAT would penalize users.
	Login RateLimitRouteConfig `yaml:"login"`
	// TrustForwardedFor honors the leftmost X-Forwarded-For entry. Only safe
	// behind a proxy that strips spoofed values.
	TrustForwardedFor bool `yaml:"trustForwardedFor"`
}

// RateLimitRouteConfig is one bucket. PerMinute is the refill rate in human
// scale units; Burst the bucket size, at least 1 or nothing is ever accepted.
type RateLimitRouteConfig struct {
	PerMinute int `yaml:"perMinute"`
	Burst     int `yaml:"burst"`
}

// AgentTokenConfig has the broker issue a federation agent's publish token for
// World and keep the raw value in Secret[Key], in the world's namespace.
type AgentTokenConfig struct {
	// World names a static worlds[] entry, the only world the token is valid in.
	World string `yaml:"world"`
	// Secret and Key locate the raw token the agent mounts.
	Secret string `yaml:"secret"`
	Key    string `yaml:"key"`
	// Paths scopes the token. Default ["/**"].
	Paths []string `yaml:"paths"`
}

// WriteScope is the path scope the gateway may write in a world; an identity
// the world allows publishes there and nowhere else. Reads are never scoped.
type WriteScope struct {
	// Paths is the glob list in the token file's pattern language, e.g.
	// ["/team-a/*"]. At least one entry is required.
	Paths []string `yaml:"paths"`
}
