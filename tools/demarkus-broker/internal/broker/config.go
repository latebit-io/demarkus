package broker

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the broker's YAML configuration. One file describes the broker's
// own runtime knobs, the OIDC provider it trusts, and the set of worlds it
// is authorized to mint tokens for. The world Tokens Secrets live in each
// world's namespace; the issuances Secret lives in the broker's namespace.
type Config struct {
	Server ServerConfig  `yaml:"server"`
	OIDC   OIDCConfig    `yaml:"oidc"`
	Worlds []WorldConfig `yaml:"worlds"`
}

// ServerConfig holds the broker's own runtime settings — listen address,
// cookie-signing key, state-cookie TTL, and the broker namespace + Secret
// name where its issuance state lives.
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
	// the issuances Secret lives here.
	BrokerNamespace string `yaml:"brokerNamespace"`
	// IssuancesSecret is the name of the broker-owned Secret holding the
	// label → {email, world, paths, ...} map (JSON-encoded). World servers
	// never read this Secret.
	IssuancesSecret string `yaml:"issuancesSecret"`
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
}

// WorldConfig describes one demarkus world the broker is authorized to
// mint tokens for. Authorization is per-world; an OIDC identity may
// qualify for some worlds and not others depending on AllowDomains.
type WorldConfig struct {
	// Name is the world's logical name (also the value in the
	// "world" field of issuances records).
	Name string `yaml:"name"`
	// Namespace is the k8s namespace where this world's TokensSecret lives.
	Namespace string `yaml:"namespace"`
	// TokensSecret is the name of the world's tokens.toml Secret. The
	// broker requires get/patch on this Secret in that namespace.
	TokensSecret string `yaml:"tokensSecret"`
	// AllowDomains is the email-domain allowlist used for authorization
	// in Slice B. A login's id_token.email must end with one of these
	// domains for the broker to mint a token for this world. Empty list
	// = no domain restriction (any verified user qualifies); the operator
	// must opt out explicitly to avoid surprise authorization.
	AllowDomains []string `yaml:"allowDomains"`
	// DefaultToken describes the capabilities of every token minted for
	// this world. Slice B treats DefaultToken as the only scope; per-call
	// narrowing is a Slice C feature.
	DefaultToken TokenScope `yaml:"defaultToken"`
}

// TokenScope is the capability bundle every token minted for a given
// world carries. Paths + Operations are written verbatim into the server-
// readable tokens.toml; ExpiresAfter is the lifetime applied to each
// minted token.
type TokenScope struct {
	// Paths is the list of glob patterns the token is allowed to access,
	// e.g. ["/team-a/*"]. Empty means no path restriction (server-side
	// default: deny). Validation: at least one entry is required to
	// avoid accidentally issuing zero-scope tokens.
	Paths []string `yaml:"paths"`
	// Operations is the set of capabilities granted, e.g.
	// ["read", "publish"]. At least one entry is required.
	Operations []string `yaml:"operations"`
	// ExpiresAfter is the lifetime of minted tokens. Zero is rejected
	// during validation — short-lived tokens are the primary identity-
	// lifecycle mechanism (see plan §6.2 revocation), so accidentally
	// issuing non-expiring tokens would silently break that property.
	ExpiresAfter time.Duration `yaml:"expiresAfter"`
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
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("broker: validate config %s: %w", path, err)
	}
	return &c, nil
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
	if c.Server.IssuancesSecret == "" {
		return fmt.Errorf("server.issuancesSecret is required")
	}
	if c.Server.StateTTL == 0 {
		c.Server.StateTTL = 5 * time.Minute
	}
	if c.Server.StateTTL < 0 {
		return fmt.Errorf("server.stateTTL must be > 0 (got %s)", c.Server.StateTTL)
	}
	if c.OIDC.Issuer == "" {
		return fmt.Errorf("oidc.issuer is required")
	}
	if c.OIDC.ClientID == "" {
		return fmt.Errorf("oidc.clientID is required")
	}
	if c.OIDC.ClientSecret == "" {
		return fmt.Errorf("oidc.clientSecret is required")
	}
	if c.OIDC.RedirectURL == "" {
		return fmt.Errorf("oidc.redirectURL is required")
	}
	if len(c.Worlds) == 0 {
		return fmt.Errorf("at least one world is required")
	}
	seen := make(map[string]bool, len(c.Worlds))
	for i := range c.Worlds {
		w := &c.Worlds[i]
		switch {
		case w.Name == "":
			return fmt.Errorf("worlds[%d]: name is required", i)
		case seen[w.Name]:
			return fmt.Errorf("worlds[%d]: duplicate name %q", i, w.Name)
		case w.Namespace == "":
			return fmt.Errorf("worlds[%d] (%s): namespace is required", i, w.Name)
		case w.TokensSecret == "":
			return fmt.Errorf("worlds[%d] (%s): tokensSecret is required", i, w.Name)
		case len(w.DefaultToken.Paths) == 0:
			return fmt.Errorf("worlds[%d] (%s): defaultToken.paths is required", i, w.Name)
		case len(w.DefaultToken.Operations) == 0:
			return fmt.Errorf("worlds[%d] (%s): defaultToken.operations is required", i, w.Name)
		case w.DefaultToken.ExpiresAfter <= 0:
			return fmt.Errorf("worlds[%d] (%s): defaultToken.expiresAfter must be > 0", i, w.Name)
		}
		seen[w.Name] = true
	}
	return nil
}
