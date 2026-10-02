package core

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// maxSweeperInterval is the short lived token lifetime; a longer sweep leaves
// expired tokens accepted for up to a whole cycle.
const maxSweeperInterval = 24 * time.Hour

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	if c.Server.BrokerNamespace == "" {
		return fmt.Errorf("server.brokerNamespace is required")
	}
	// The GCS driver validates the name when the broker opens the bucket.
	if !strings.HasPrefix(c.Server.StateBucket, "gs://") || c.Server.StateBucketName() == "" {
		return fmt.Errorf("server.stateBucket %q must be a gs:// bucket URL", c.Server.StateBucket)
	}
	if err := c.Server.normalizePublicURLs(); err != nil {
		return err
	}
	if c.Server.StateTTL == 0 {
		c.Server.StateTTL = 5 * time.Minute
	}
	if c.Server.StateTTL < 0 {
		return fmt.Errorf("server.stateTTL must be > 0 (got %s)", c.Server.StateTTL)
	}
	if err := c.Server.ApplyDefaults(); err != nil {
		return err
	}
	if err := c.Server.validateGateways(); err != nil {
		return err
	}
	if err := c.OIDC.validate(); err != nil {
		return err
	}
	if err := c.Provisioning.validate(); err != nil {
		return err
	}
	if c.Provisioning.Enabled() && !c.Server.Memory.Enabled() {
		return fmt.Errorf("provisioning.mode %q needs the memory gateway (server.memory.publicURL)", c.Provisioning.Mode)
	}
	if err := c.validateWorlds(); err != nil {
		return err
	}
	if err := c.validateFederation(); err != nil {
		return err
	}
	if err := validateWebClients(c.WebClients); err != nil {
		return err
	}
	if err := c.Sweeper.applyDefaultsAndValidate(); err != nil {
		return err
	}
	return c.RateLimit.applyDefaultsAndValidate()
}

// validateWorlds checks every static world and the set as a whole. Zero
// worlds is legal with provisioning on.
func (c *Config) validateWorlds() error {
	if len(c.Worlds) == 0 && !c.Provisioning.Enabled() {
		return fmt.Errorf("at least one world is required (or enable provisioning)")
	}
	seen := make(map[string]bool, len(c.Worlds))
	for i := range c.Worlds {
		w := &c.Worlds[i]
		if err := validateWorld(i, w); err != nil {
			return err
		}
		if err := c.validateWorldProfile(i, w); err != nil {
			return err
		}
		if seen[w.Name] {
			return fmt.Errorf("worlds[%d]: duplicate name %q", i, w.Name)
		}
		seen[w.Name] = true
	}
	return nil
}

// applyDefaultsAndValidate fills the sweeper knobs and bounds the interval.
func (s *SweeperConfig) applyDefaultsAndValidate() error {
	if s.Interval == 0 {
		s.Interval = 5 * time.Minute
	}
	if s.Interval < 0 {
		return fmt.Errorf("sweeper.interval must be > 0 (got %s)", s.Interval)
	}
	if s.Interval > maxSweeperInterval {
		return fmt.Errorf("sweeper.interval must be <= %s (got %s); intervals longer than a token lifetime defeat expiry-driven revocation", maxSweeperInterval, s.Interval)
	}
	if s.LeaseName == "" {
		// Legacy name pinned: a renamed Lease would briefly dual-run
		// sweepers across an in-place upgrade.
		s.LeaseName = "demarkus-broker-sweeper"
	}
	return nil
}

// validateFederation fills the federation knobs; the hub must be a static
// knowledge world served in process, the only place its grant can work.
func (c *Config) validateFederation() error {
	f := &c.Federation
	if f.Hub == "" {
		return nil
	}
	if !slices.Contains(c.FederatedWorlds(), f.Hub) {
		return fmt.Errorf("federation.hub %q must name a local knowledge world", f.Hub)
	}
	f.LeaseName = cmp.Or(f.LeaseName, "demarkus-federation")
	f.QuietPeriod = cmp.Or(f.QuietPeriod, 30*time.Second)
	f.Interval = cmp.Or(f.Interval, time.Minute)
	if f.QuietPeriod < 0 || f.Interval < 0 {
		return fmt.Errorf("federation.quietPeriod and federation.interval must not be negative (got %s, %s); zero takes the default", f.QuietPeriod, f.Interval)
	}
	return nil
}

// validateWorldProfile settles which gateway serves the world. A memory world
// names its tenant, or identity = world resolution is ambiguous; a mixed
// deployment states every profile so a tenant world cannot default to org reads.
func (c *Config) validateWorldProfile(i int, w *WorldConfig) error {
	w.Profile = strings.ToLower(strings.TrimSpace(w.Profile))
	switch w.Profile {
	case "":
		if c.Server.Memory.Enabled() {
			return fmt.Errorf("worlds[%d] (%s): profile is required (knowledge or memory) when server.memory is configured", i, w.Name)
		}
		w.Profile = ProfileKnowledge
	case ProfileKnowledge:
	case ProfileMemory:
		if !c.Server.Memory.Enabled() {
			return fmt.Errorf("worlds[%d] (%s): profile memory needs the memory gateway (server.memory.publicURL)", i, w.Name)
		}
		if w.Allow.Empty() {
			return fmt.Errorf("worlds[%d] (%s): allow must not be empty in a memory world; each is provisioned for one identity", i, w.Name)
		}
	default:
		return fmt.Errorf("worlds[%d] (%s): profile must be %q or %q (got %q)", i, w.Name, ProfileKnowledge, ProfileMemory, w.Profile)
	}
	return nil
}

// validate requires every IdP field. A configured signing key is parsed so a
// malformed PEM fails here with its field name, not after discovery and kube
// setup; a blank key is generated at startup (EnsureSigningKey).
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
	}
	if o.BrokerSigningKey != "" {
		if _, err := NewIDTokenSigner([]byte(o.BrokerSigningKey)); err != nil {
			return fmt.Errorf("oidc.brokerSigningKey is invalid: %w", err)
		}
	}
	return normalizeList("oidc.allowDomains", o.AllowDomains)
}

// normalizePublicURL trims whitespace + trailing slash, lowercases scheme
// and host, and enforces absolute-URL shape with no query/fragment — a bad
// value renders broken URLs into discovery metadata. Empty passes through.
func normalizePublicURL(field, raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%s must be an absolute URL (got %q)", field, trimmed)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must not carry a query or fragment (got %q)", field, trimmed)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// normalizePublicURLs canonicalizes the advertised base URLs at load.
// publicURL is required (the issuer); mcp.publicURL falls back to it in
// ServerConfig.Gateway; memory.publicURL stays blank when the gateway is off.
func (s *ServerConfig) normalizePublicURLs() error {
	var err error
	if s.PublicURL, err = normalizePublicURL("server.publicURL", s.PublicURL); err != nil {
		return err
	}
	if s.PublicURL == "" {
		return fmt.Errorf("server.publicURL is required")
	}
	if s.MCP.PublicURL, err = normalizePublicURL("server.mcp.publicURL", s.MCP.PublicURL); err != nil {
		return err
	}
	s.Memory.PublicURL, err = normalizePublicURL("server.memory.publicURL", s.Memory.PublicURL)
	return err
}

// validateGateways fills both gateways' defaults. The memory gateway is
// selected by Host, so its hostname must differ from the knowledge gateway's.
func (s *ServerConfig) validateGateways() error {
	if err := s.MCP.validate("server.mcp"); err != nil {
		return err
	}
	if !s.Memory.Enabled() {
		return nil
	}
	if err := s.Memory.validate("server.memory"); err != nil {
		return err
	}
	if knowledge := s.Gateway(ProfileKnowledge); s.Memory.Host() == knowledge.Host() {
		return fmt.Errorf("server.memory.publicURL must be on a different host than server.mcp.publicURL (both are %q)", s.Memory.Host())
	}
	return nil
}

// validate fills the tool profile default and rejects an unknown one.
func (g *GatewayConfig) validate(field string) error {
	if g.ToolProfile == "" {
		g.ToolProfile = mcpfmt.ProfileFull
	}
	if err := mcpfmt.ValidProfile(g.ToolProfile); err != nil {
		return fmt.Errorf("%s.toolProfile: %w", field, err)
	}
	return nil
}

// validateWorld owns the checks on one world and normalizes its allow lists
// in place; the caller owns the checks across worlds, such as duplicates.
func validateWorld(i int, w *WorldConfig) error {
	switch {
	case w.Name == "":
		return fmt.Errorf("worlds[%d]: name is required", i)
	case !protocol.IsWorldName(w.Name):
		return fmt.Errorf("worlds[%d]: name %q must be a DNS label: lowercase letters, digits and hyphens, at most 63, no hyphen at either end", i, w.Name)
	case len(w.WriteScope.Paths) == 0:
		return fmt.Errorf("worlds[%d] (%s): writeScope.paths is required", i, w.Name)
	}
	for _, pattern := range w.WriteScope.Paths {
		if err := protocol.ValidatePathPattern(pattern); err != nil {
			return fmt.Errorf("worlds[%d] (%s): writeScope.paths %q: %w", i, w.Name, pattern, err)
		}
	}
	w.Namespace = strings.ToLower(strings.TrimSpace(w.Namespace))
	if w.Namespace == "" && w.InternalAddress == "" {
		return fmt.Errorf("worlds[%d] (%s): namespace or internalAddress is required", i, w.Name)
	}
	// Lowercased and trimmed at load so authorization is a plain compare.
	// An empty entry is a typo and surfaces here rather than never matching.
	allow := fmt.Sprintf("worlds[%d] (%s): allow.", i, w.Name)
	if err := normalizeList(allow+"domains", w.Allow.Domains); err != nil {
		return err
	}
	if err := normalizeList(allow+"emails", w.Allow.Emails); err != nil {
		return err
	}
	return normalizeList(allow+"groups", w.Allow.Groups)
}

// validateWebClients enforces the confidential client registry and normalizes
// secret hashes; a cleartext secret is hashed and cleared. A half registered
// client would surface as a runtime auth failure instead of a startup error.
func validateWebClients(clients []WebClientConfig) error {
	seen := make(map[string]bool, len(clients))
	for i := range clients {
		wc := &clients[i]
		if wc.ClientID == "" {
			return fmt.Errorf("webClients[%d]: clientID is required", i)
		}
		if seen[wc.ClientID] {
			return fmt.Errorf("webClients[%d]: duplicate clientID %q", i, wc.ClientID)
		}
		seen[wc.ClientID] = true
		if wc.ClientSecret != "" {
			if wc.ClientSecretHash != "" {
				return fmt.Errorf("webClients[%d] (%s): clientSecret and clientSecretHash are mutually exclusive", i, wc.ClientID)
			}
			wc.ClientSecretHash = HashClientSecret(wc.ClientSecret)
			wc.ClientSecret = ""
		} else if wc.ClientSecretEnv != "" && wc.ClientSecretHash == "" {
			return fmt.Errorf("webClients[%d] (%s): clientSecretEnv %s is unset or empty", i, wc.ClientID, wc.ClientSecretEnv)
		}
		// Lowercased so the constant time compare never misses an uppercase paste.
		wc.ClientSecretHash = strings.ToLower(strings.TrimSpace(wc.ClientSecretHash))
		if !isSHA256Hex(wc.ClientSecretHash) {
			return fmt.Errorf("webClients[%d] (%s): clientSecret or clientSecretHash (64 hex chars, sha256 of the secret) is required", i, wc.ClientID)
		}
		if len(wc.RedirectURIs) == 0 {
			return fmt.Errorf("webClients[%d] (%s): at least one redirectURI is required", i, wc.ClientID)
		}
		for j, raw := range wc.RedirectURIs {
			if err := ValidateWebRedirectURI(raw); err != nil {
				return fmt.Errorf("webClients[%d] (%s): redirectURIs[%d]: %w", i, wc.ClientID, j, err)
			}
		}
	}
	return nil
}

// ValidateWebRedirectURI requires an absolute https URL without userinfo or
// fragment. Loopback is rejected too: that client belongs on the native path.
func ValidateWebRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("%q: scheme must be https", raw)
	case u.Host == "":
		return fmt.Errorf("%q: host is required", raw)
	case u.User != nil:
		return fmt.Errorf("%q: userinfo is not allowed", raw)
	case u.Fragment != "" || u.RawFragment != "":
		return fmt.Errorf("%q: fragment is not allowed", raw)
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return fmt.Errorf("%q: loopback hosts use the native-app flow, not the web-client registry", raw)
	}
	return nil
}

// isSHA256Hex reports whether s is exactly 64 lowercase hex chars, so a
// truncated or plaintext secret fails at load.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// normalizeList lowercases and trims an allowlist in place; an empty entry
// is a typo and fails here rather than never matching.
func normalizeList(field string, list []string) error {
	for j, s := range list {
		norm := strings.ToLower(strings.TrimSpace(s))
		if norm == "" {
			return fmt.Errorf("%s[%d] is empty", field, j)
		}
		list[j] = norm
	}
	return nil
}
