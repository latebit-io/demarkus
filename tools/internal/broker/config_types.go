package broker

import (
	"slices"
	"sync"
	"time"
)

// Config is the broker's YAML configuration: its own runtime knobs, the OIDC
// provider it trusts and the worlds it mints tokens for. World Tokens Secrets
// live in each world's namespace; the broker's own state in its namespace.
type Config struct {
	Server       ServerConfig       `yaml:"server"`
	OIDC         OIDCConfig         `yaml:"oidc"`
	Storage      StorageConfig      `yaml:"storage"`
	Worlds       []WorldConfig      `yaml:"worlds"`
	WebClients   []WebClientConfig  `yaml:"webClients"`
	Sweeper      SweeperConfig      `yaml:"sweeper"`
	RateLimit    RateLimitConfig    `yaml:"rateLimit"`
	WorldDialer  WorldDialerConfig  `yaml:"worldDialer"`
	Provisioning ProvisioningConfig `yaml:"provisioning"`

	// registry is the live world set, built from Worlds on first use.
	registryOnce sync.Once
	registry     *worldRegistry
}

// worlds is the live world set: Worlds as loaded, plus what provisioning adds.
func (c *Config) worlds() *worldRegistry {
	c.registryOnce.Do(func() { c.registry = newWorldRegistry(c.Worlds) })
	return c.registry
}

const (
	storageBackendKubernetes = "kubernetes"
	storageBackendFile       = "file"
)

// StorageConfig selects the credential backend. "kubernetes" (the default)
// keeps everything in Secrets; "file" is single-host mode, where broker state
// lives under Dir and world token hashes go into each world's tokensFile.
type StorageConfig struct {
	Backend string `yaml:"backend"`
	// Dir is the broker's state directory in file mode. Required then.
	Dir string `yaml:"dir"`
}

func (c *Config) fileBackend() bool {
	return c.Storage.Backend == storageBackendFile
}

// FileBackend reports whether the broker runs in single-host file mode.
func (c *Config) FileBackend() bool { return c.fileBackend() }

// WebClientConfig registers one confidential web client (RFC 6749 2.1): a
// server side app with a secret and a real https redirect. The list is operator
// curated; /register stays public-client-only and never adds to it.
type WebClientConfig struct {
	// ClientID is presented on /oauth/authorize and the token endpoint.
	ClientID string `yaml:"clientID"`
	// ClientSecretHash is the lowercase sha256 hex of the client secret. sha256
	// suffices because the secret is operator generated randomness, not a password.
	ClientSecretHash string `yaml:"clientSecretHash"`
	// RedirectURIs is the exact match allowlist: absolute https URLs, no
	// wildcards, no loopback exemption. Anything looser is an open redirect.
	RedirectURIs []string `yaml:"redirectURIs"`
	// Name is an optional human label for logs.
	Name string `yaml:"name"`
}

// webClient returns the registered confidential client for clientID, or false
// when the id is unregistered (the public/native path).
func (c *Config) webClient(clientID string) (*WebClientConfig, bool) {
	for i := range c.WebClients {
		if c.WebClients[i].ClientID == clientID {
			return &c.WebClients[i], true
		}
	}
	return nil, false
}

// allowsRedirect is a byte for byte compare per OAuth 2.1; normalization
// (case, default ports, dot segments) is how redirect allowlists get bypassed.
func (wc *WebClientConfig) allowsRedirect(raw string) bool {
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
	// CookieKey is the base64 HMAC key signing the OIDC state cookie. Rotating
	// it invalidates in-flight logins, which is the wanted effect.
	CookieKey string `yaml:"cookieKey"`
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
	// DynamicClientsSecret holds the RFC 7591 registration map.
	DynamicClientsSecret string `yaml:"dynamicClientsSecret"`
	// RefreshTokenTTL is the lifetime of a new refresh token. Default 90 days.
	RefreshTokenTTL time.Duration `yaml:"refreshTokenTTL"`
	// IDTokenTTL is the lifetime of a broker signed id_token. Default 15m.
	IDTokenTTL time.Duration `yaml:"idTokenTTL"`
	// Realm names the product in the gateway's WWW-Authenticate challenge;
	// each binary defaults its own before NewServer.
	Realm string `yaml:"realm"`
	// MCP is the gateway listener, on its own Addr, sharing the auth and
	// rate limit machinery with the management API.
	MCP MCPConfig `yaml:"mcp"`
}

// MCPConfig governs the MCP over HTTPS gateway listener. Reads dispatch with
// an empty bearer; writes use a per world token minted on first write.
type MCPConfig struct {
	// Addr is the gateway listen address, default ":8081". Must differ from
	// Server.Addr; the chart routes the two surfaces separately.
	Addr string `yaml:"addr"`
	// PublicURL is the gateway's base URL when split-host ingress gives it a
	// hostname other than the issuer's. Feeds the RFC 9728 resource metadata.
	PublicURL string `yaml:"publicURL"`
	// ToolProfile selects the tool surface: "lean" omits operator and
	// federation tools; "full" (default) keeps every tool.
	ToolProfile string `yaml:"toolProfile"`
	// TLS terminates HTTPS at the broker when both files are set.
	TLS MCPTLSConfig `yaml:"tls"`
	// FirstMint* tune the retry loop that absorbs kubelet Secret propagation
	// lag after a lazy token mint, when a dispatch on the new token answers
	// unauthorized. Backoff doubles from Initial up to Max.
	FirstMintMaxAttempts    int           `yaml:"firstMintMaxAttempts"`
	FirstMintInitialBackoff time.Duration `yaml:"firstMintInitialBackoff"`
	FirstMintMaxBackoff     time.Duration `yaml:"firstMintMaxBackoff"`
}

// MCPTLSConfig is the optional cert and key pair for the gateway listener.
// Set both or neither.
type MCPTLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
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
	// broker id_tokens. Supplied through a Secret, never helm values.
	BrokerSigningKey string `yaml:"brokerSigningKey"`
	// AllowDomains is the Google Workspace hosted domain allowlist, checked on
	// every IdP exchange before any code or token is issued. Keyed on the `hd`
	// claim, which the user cannot influence; consumer accounts have none.
	AllowDomains []string `yaml:"allowDomains"`
}

// WorldConfig describes one world the broker mints tokens for.
// Authorization is per world through Allow.
type WorldConfig struct {
	// generation tells one provisioning of a name from the next, so state
	// cached for a deprovisioned world is not reused by its successor.
	generation string
	// Name keys tool calls and the write token store.
	Name string `yaml:"name"`
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
	// TokensSecretKey overrides the data key in TokensSecret (default
	// "tokens.toml"); dynamic tenants share one Secret with a key per world.
	TokensSecretKey string `yaml:"tokensSecretKey"`
	// TokensFile is the world server's tokens.toml path, file backend only.
	TokensFile string `yaml:"tokensFile"`
	// Allow is the per world authorization predicate; all lists empty admits
	// any verified identity.
	Allow AllowConfig `yaml:"allow"`
	// DefaultToken is the path scope of the broker's write token for this world.
	DefaultToken TokenScope `yaml:"defaultToken"`
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

// TokenScope is the path scope of the broker's write token for a world. The
// operations are always ["publish"], hardcoded so config cannot open reads.
type TokenScope struct {
	// Paths is the glob list, e.g. ["/team-a/*"]. At least one entry is required.
	Paths []string `yaml:"paths"`
}
