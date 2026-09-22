package core

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/mcpfmt"
)

// maxSweeperInterval is the short lived token lifetime; a longer sweep leaves
// expired tokens accepted for up to a whole cycle.
const maxSweeperInterval = 24 * time.Hour

// WorldNameRE is a DNS label. A world name is the host of every tool URL, a
// graph key (hosts compare lowercase) and part of a Secret name, so nothing
// looser works everywhere. Rejected, never normalized: that would rename Secrets.
var WorldNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	if c.Server.CookieKey == "" {
		return fmt.Errorf("server.cookieKey is required")
	}
	if err := c.validateStorage(); err != nil {
		return err
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
	if err := c.Server.applyDeviceFlowDefaults(); err != nil {
		return err
	}
	if err := c.Server.applyRefreshDefaults(); err != nil {
		return err
	}
	if err := c.Server.MCP.validate(); err != nil {
		return err
	}
	// Caught here so the typo reads as such, not as a bind error at startup.
	if c.Server.MCP.Addr == c.Server.Addr {
		return fmt.Errorf("server.mcp.addr must differ from server.addr (both are %q)", c.Server.Addr)
	}
	if err := c.OIDC.validate(); err != nil {
		return err
	}
	// Zero static worlds is legal when provisioning is enabled: the
	// whole world set then arrives dynamically via the registry.
	if len(c.Worlds) == 0 && !c.Provisioning.Enabled() {
		return fmt.Errorf("at least one world is required (or enable provisioning)")
	}
	seen := make(map[string]bool, len(c.Worlds))
	seenSecretRefs := make(map[[2]string]string, len(c.Worlds))
	for i := range c.Worlds {
		w := &c.Worlds[i]
		if err := validateWorld(i, w, c.fileBackend()); err != nil {
			return err
		}
		if seen[w.Name] {
			return fmt.Errorf("worlds[%d]: duplicate name %q", i, w.Name)
		}
		seen[w.Name] = true
		if !c.fileBackend() {
			ref := [2]string{w.Namespace, w.TokensSecret}
			if other, ok := seenSecretRefs[ref]; ok {
				return fmt.Errorf("worlds[%d] (%s): duplicate tokens Secret reference %q (also used by world %q)", i, w.Name, fmt.Sprintf("%s/%s", ref[0], ref[1]), other)
			}
			seenSecretRefs[ref] = w.Name
		}
	}
	if err := validateWebClients(c.WebClients); err != nil {
		return err
	}
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
		// Legacy name pinned: a renamed Lease would briefly dual-run
		// sweepers across an in-place upgrade.
		c.Sweeper.LeaseName = "demarkus-broker-sweeper"
	}
	return c.RateLimit.applyDefaultsAndValidate()
}

// validateStorage normalizes the backend selection and enforces what each
// backend needs: a state dir in file mode, the broker namespace otherwise.
func (c *Config) validateStorage() error {
	switch c.Storage.Backend {
	case "":
		c.Storage.Backend = StorageBackendKubernetes
	case StorageBackendKubernetes, StorageBackendFile:
	default:
		return fmt.Errorf("storage.backend must be %q or %q (got %q)", StorageBackendKubernetes, StorageBackendFile, c.Storage.Backend)
	}
	if c.fileBackend() {
		if c.Storage.Dir == "" {
			return fmt.Errorf("storage.dir is required when storage.backend is %q", StorageBackendFile)
		}
		return c.validateFilePaths()
	}
	if c.Server.BrokerNamespace == "" {
		return fmt.Errorf("server.brokerNamespace is required")
	}
	return nil
}

// validateFilePaths rejects tokensFile values aliasing each other or a
// broker-state file: two stores mutating one document corrupts it.
func (c *Config) validateFilePaths() error {
	seen := map[string]string{
		filepath.Clean(RefreshTokensRef(c).Path):  "storage.dir refresh-tokens state",
		filepath.Clean(DynamicClientsRef(c).Path): "storage.dir dynamic-clients state",
	}
	for i := range c.Worlds {
		w := &c.Worlds[i]
		seen[filepath.Clean(WorldWriteTokenRef(c, w.Name).Path)] = fmt.Sprintf("storage.dir write-token state for world %q", w.Name)
	}
	for i := range c.Worlds {
		w := &c.Worlds[i]
		if w.TokensFile == "" {
			continue
		}
		p := filepath.Clean(w.TokensFile)
		if other, ok := seen[p]; ok {
			return fmt.Errorf("worlds[%d] (%s): tokensFile %q collides with %s", i, w.Name, w.TokensFile, other)
		}
		seen[p] = fmt.Sprintf("tokensFile of world %q", w.Name)
	}
	return nil
}

// validate requires every IdP field. The signing key is parsed, not only
// checked for presence, so a malformed PEM fails here with its field name
// instead of after discovery and kube setup have already run.
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
	return normalizeList("oidc.allowDomains", o.AllowDomains)
}

// normalizePublicURL trims whitespace + trailing slash and enforces
// absolute-URL shape with no query/fragment — a bad value renders broken
// URLs into discovery metadata. Empty passes through; required is the caller's rule.
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
	return trimmed, nil
}

// normalizePublicURLs canonicalizes the advertised base URLs at load.
// publicURL is required (the issuer); mcp.publicURL falls back to it.
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
	if s.MCP.PublicURL == "" {
		s.MCP.PublicURL = s.PublicURL
	}
	return nil
}

// validate fills the gateway defaults and rejects a half set TLS pair, which
// would otherwise fail at listen or silently serve plain HTTP.
func (m *MCPConfig) validate() error {
	if m.Addr == "" {
		m.Addr = defaultMCPAddr
	}
	if m.ToolProfile == "" {
		m.ToolProfile = mcpfmt.ProfileFull
	}
	if err := mcpfmt.ValidProfile(m.ToolProfile); err != nil {
		return fmt.Errorf("server.mcp.toolProfile: %w", err)
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

// validateWorld owns the checks on one world and normalizes its allow lists
// in place; the caller owns the checks across worlds, such as duplicates.
func validateWorld(i int, w *WorldConfig, fileMode bool) error {
	switch {
	case w.Name == "":
		return fmt.Errorf("worlds[%d]: name is required", i)
	case !WorldNameRE.MatchString(w.Name):
		return fmt.Errorf("worlds[%d]: name %q must be a DNS label: lowercase letters, digits and hyphens, at most 63, no hyphen at either end", i, w.Name)
	case len(w.DefaultToken.Paths) == 0:
		return fmt.Errorf("worlds[%d] (%s): defaultToken.paths is required", i, w.Name)
	}
	// Kubernetes worlds live in a namespace with a tokens Secret; file mode
	// worlds need a local tokens.toml and an explicit address, since the
	// cluster DNS default means nothing off cluster.
	if fileMode {
		switch {
		case w.TokensFile == "":
			return fmt.Errorf("worlds[%d] (%s): tokensFile is required in file-backend mode", i, w.Name)
		case w.InternalAddress == "":
			return fmt.Errorf("worlds[%d] (%s): internalAddress is required in file-backend mode", i, w.Name)
		}
	} else {
		w.Namespace = strings.ToLower(strings.TrimSpace(w.Namespace))
		w.TokensSecret = strings.ToLower(strings.TrimSpace(w.TokensSecret))
		switch {
		case w.Namespace == "":
			return fmt.Errorf("worlds[%d] (%s): namespace is required", i, w.Name)
		case w.TokensSecret == "":
			return fmt.Errorf("worlds[%d] (%s): tokensSecret is required", i, w.Name)
		}
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
// secret hashes. A half registered client would present as a confusing
// runtime auth failure instead of a startup error.
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
		// Lowercased so the constant time compare never misses an uppercase paste.
		wc.ClientSecretHash = strings.ToLower(strings.TrimSpace(wc.ClientSecretHash))
		if !isSHA256Hex(wc.ClientSecretHash) {
			return fmt.Errorf("webClients[%d] (%s): clientSecretHash must be 64 hex chars (sha256 of the client secret)", i, wc.ClientID)
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
