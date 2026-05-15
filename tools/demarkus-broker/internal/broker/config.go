package broker

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the broker's YAML configuration. One file describes the broker's
// own runtime knobs, the OIDC provider it trusts, and the set of worlds it
// is authorized to mint tokens for. The world Tokens Secrets live in each
// world's namespace; the issuances Secret lives in the broker's namespace.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	OIDC      OIDCConfig      `yaml:"oidc"`
	Worlds    []WorldConfig   `yaml:"worlds"`
	Sweeper   SweeperConfig   `yaml:"sweeper"`
	RateLimit RateLimitConfig `yaml:"rateLimit"`
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
	// InsecureCookies drops the Secure attribute on the OIDC state cookie.
	// Default false (production-correct: state cookies travel over HTTPS
	// only). Flip to true ONLY for kind / local dev where the broker is
	// reachable over plain HTTP and the Secure attribute would prevent the
	// cookie jar from replaying it on /auth/callback. Enabling this in a
	// real deployment turns the state cookie into a downgrade attack vector
	// — any plaintext-hijacked redirect can complete the OIDC dance.
	InsecureCookies bool `yaml:"insecureCookies"`
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
// qualify for some worlds and not others depending on Allow.
type WorldConfig struct {
	// Name is the world's logical name (also the value in the
	// "world" field of issuances records).
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
	// Allow is the per-world authorization predicate. See AllowConfig for
	// the rules. When every list is empty the world accepts any verified
	// identity (back-compat with pre-Slice-C configs that had only
	// AllowDomains and left it empty).
	Allow AllowConfig `yaml:"allow"`
	// DefaultToken describes the capabilities of every token minted for
	// this world. Slice B treats DefaultToken as the only scope; per-call
	// narrowing is a Slice C feature.
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

// SweeperConfig knobs control the broker's periodic expiry+drift
// janitor (Slice C.2). The sweeper runs by default in every broker;
// multi-replica deployments need it so expired tokens age out and
// operator hand-edits of world tokens.toml propagate back into the
// issuances Secret. Leader election timings are not yet exposed —
// client-go defaults (15s lease, 10s renew, 2s retry) suffice for our
// failover budget and revisit only if a customer needs faster failover.
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
// Defaults come from plan §6.2 Slice C.4: 10/min subject on /tokens,
// /tokens/:label DELETE, /tokens/:label/rotate (single shared bucket,
// burst 5); 20/min IP on /auth/login (burst 5). Operators tighten or
// loosen via YAML.
type RateLimitConfig struct {
	// Disabled is the opt-out switch, same naming convention as
	// SweeperConfig.Disabled. Zero-value (false) means the limiter
	// runs with the configured rates, so an operator who omits the
	// rateLimit block in their values file still gets the protection.
	Disabled bool `yaml:"disabled"`
	// Tokens governs the per-subject bucket shared across GET
	// /tokens, DELETE /tokens/:label, and POST /tokens/:label/rotate.
	// One shared bucket means a misbehaving client cannot multiply
	// its effective rate by fanning out across the three routes.
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
	c.applyEnvOverrides()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("broker: validate config %s: %w", path, err)
	}
	return &c, nil
}

// applyEnvOverrides lets a small set of high-sensitivity fields come from
// environment variables instead of the on-disk config. Today only
// OIDC_CLIENT_SECRET is supported, so production deployments can keep the
// OAuth client secret in an externally-managed Kubernetes Secret (External
// Secrets Operator, Sealed Secrets, Vault) mounted via secretKeyRef rather
// than baked into the chart-rendered config Secret where it would leak
// into helm release history. The env var wins over the file value when
// both are set; an empty env var is treated as unset so an accidentally
// cleared variable doesn't blank out a file-supplied value at runtime.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("OIDC_CLIENT_SECRET"); v != "" {
		c.OIDC.ClientSecret = v
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
	// interval longer than the typical 24h token lifetime would let
	// expired tokens linger past their `defaultToken.expiresAfter`
	// window — defeating the short-lived-tokens identity-lifecycle
	// model (see plan §Revocation §3).
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
	case len(w.DefaultToken.Operations) == 0:
		return fmt.Errorf("worlds[%d] (%s): defaultToken.operations is required", i, w.Name)
	case w.DefaultToken.ExpiresAfter <= 0:
		return fmt.Errorf("worlds[%d] (%s): defaultToken.expiresAfter must be > 0", i, w.Name)
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
