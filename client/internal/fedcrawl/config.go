// Package fedcrawl provides multi-server federation crawling for the Mark Protocol.
// It discovers servers, collects content hashes, and publishes indexes to hubs.
package fedcrawl

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/latebit-io/demarkus/client/fetch"
)

// Config holds the crawler configuration loaded from a TOML file.
type Config struct {
	Seeds []string `toml:"seeds"` // Initial servers to crawl (mark://host:port)
	Hubs  []string `toml:"hubs"`  // Servers to publish indexes to
	// Endpoints maps logical Mark authorities to transport routes. This lets
	// broker-style world names remain graph/token identity while agents dial
	// cluster-internal addresses with the SNI required by a shared listener.
	Endpoints map[string]EndpointConfig `toml:"endpoints"`

	Crawl      CrawlConfig      `toml:"crawl"`
	Schedule   ScheduleConfig   `toml:"schedule"`
	Politeness PolitenessConfig `toml:"politeness"`
	Publish    PublishConfig    `toml:"publish"`
}

// EndpointConfig is the TOML representation of an explicit transport endpoint.
type EndpointConfig struct {
	DialAddress string `toml:"dial_address"`
	ServerName  string `toml:"server_name"`
}

// CrawlConfig controls the scope and depth of crawling.
type CrawlConfig struct {
	MaxDepth     int `toml:"max_depth"`     // Max directory nesting per server (default: 8); the cycle-safety bound: self-referencing listings mint ever-deeper paths the visited set cannot catch
	MaxServers   int `toml:"max_servers"`   // Max servers to discover (default: 50)
	MaxDocuments int `toml:"max_documents"` // Max documents per crawl run (default: 1000)
	Workers      int `toml:"workers"`       // Concurrent fetch goroutines (default: 5)
}

// ScheduleConfig controls daemon mode scheduling.
type ScheduleConfig struct {
	Interval time.Duration `toml:"interval"` // Time between crawl runs (default: 6h)
}

// PolitenessConfig controls request rate limiting.
type PolitenessConfig struct {
	RequestDelay         time.Duration `toml:"request_delay"`          // Delay between requests to same server (default: 100ms)
	PerServerConcurrency int           `toml:"per_server_concurrency"` // Max concurrent requests per server (default: 2)
}

// PublishConfig controls hub publications (hash indexes and /graph.md).
type PublishConfig struct {
	// Retention caps each published artifact's version history to its newest
	// N versions — the server prunes older versions on write (retention
	// publish metadata, SPEC §9.9). Everything the agent publishes is
	// regenerated wholesale on every run, so old versions are pure growth
	// (one live graph document reached 545 versions before this existed).
	// 0 keeps every version. (default: 20)
	Retention int `toml:"retention"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Crawl: CrawlConfig{
			MaxDepth:     8,
			MaxServers:   50,
			MaxDocuments: 1000,
			Workers:      5,
		},
		Schedule: ScheduleConfig{
			Interval: 6 * time.Hour,
		},
		Politeness: PolitenessConfig{
			RequestDelay:         100 * time.Millisecond,
			PerServerConcurrency: 2,
		},
		Publish: PublishConfig{
			Retention: 20,
		},
	}
}

// Load reads a TOML config file and returns a Config.
// If the file does not exist, returns DefaultConfig with the given seeds.
// Missing fields inherit from defaults.
func Load(path string, seeds []string) (Config, error) {
	cfg := DefaultConfig()

	if path == "" {
		if len(seeds) > 0 {
			cfg.Seeds = seeds
		}
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if len(seeds) > 0 {
				cfg.Seeds = seeds
			}
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	// CLI seeds override config file (applied after TOML decode to ensure precedence).
	if len(seeds) > 0 {
		cfg.Seeds = seeds
	}

	return cfg, nil
}

// Validate checks that the configuration is valid.
func (c *Config) Validate() error {
	if len(c.Seeds) == 0 {
		return fmt.Errorf("at least one seed server is required")
	}
	seeds, err := normalizeServerURLs("seed", c.Seeds)
	if err != nil {
		return err
	}
	hubs, err := normalizeServerURLs("hub", c.Hubs)
	if err != nil {
		return err
	}
	c.Seeds = seeds
	c.Hubs = hubs
	if err := c.normalizeEndpoints(); err != nil {
		return err
	}
	if c.Crawl.MaxDepth < 0 {
		return fmt.Errorf("crawl.max_depth must be non-negative")
	}
	if c.Crawl.MaxServers < 0 {
		return fmt.Errorf("crawl.max_servers must be non-negative")
	}
	if c.Crawl.MaxDocuments < 0 {
		return fmt.Errorf("crawl.max_documents must be non-negative")
	}
	if c.Crawl.Workers < 1 {
		return fmt.Errorf("crawl.workers must be at least 1")
	}
	if c.Schedule.Interval < 0 {
		return fmt.Errorf("schedule.interval must be non-negative")
	}
	if c.Politeness.RequestDelay < 0 {
		return fmt.Errorf("politeness.request_delay must be non-negative")
	}
	if c.Politeness.PerServerConcurrency < 1 {
		return fmt.Errorf("politeness.per_server_concurrency must be at least 1")
	}
	if c.Publish.Retention < 0 {
		return fmt.Errorf("publish.retention must be non-negative (0 keeps every version)")
	}
	return nil
}

func normalizeServerURLs(kind string, values []string) ([]string, error) {
	normalized := make([]string, len(values))
	for i, raw := range values {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "mark" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%s %d %q must be a mark:// server URL", kind, i, raw)
		}
		host, path, err := fetch.ParseMarkURL(raw)
		if err != nil {
			return nil, fmt.Errorf("%s %d %q: %w", kind, i, raw, err)
		}
		if path != "/" {
			return nil, fmt.Errorf("%s %d %q must not contain a document path", kind, i, raw)
		}
		normalized[i] = "mark://" + host
	}
	return normalized, nil
}

// ClientEndpoints converts validated crawler endpoints to fetch transport
// endpoints. Validate must run first so keys and dial addresses are canonical.
func (c *Config) ClientEndpoints() map[string]fetch.Endpoint {
	if len(c.Endpoints) == 0 {
		return nil
	}
	endpoints := make(map[string]fetch.Endpoint, len(c.Endpoints))
	for authority, endpoint := range c.Endpoints {
		endpoints[authority] = fetch.Endpoint{
			DialAddress: endpoint.DialAddress,
			ServerName:  endpoint.ServerName,
		}
	}
	return endpoints
}

func (c *Config) normalizeEndpoints() error {
	if len(c.Endpoints) == 0 {
		return nil
	}
	normalized := make(map[string]EndpointConfig, len(c.Endpoints))
	for rawAuthority, endpoint := range c.Endpoints {
		authority, err := normalizeAuthority(rawAuthority)
		if err != nil {
			return fmt.Errorf("endpoint authority %q: %w", rawAuthority, err)
		}
		if _, duplicate := normalized[authority]; duplicate {
			return fmt.Errorf("endpoint authority %q duplicates normalized authority %q", rawAuthority, authority)
		}
		dialAddress, err := normalizeAuthority(endpoint.DialAddress)
		if err != nil {
			return fmt.Errorf("endpoint %q dial_address: %w", rawAuthority, err)
		}
		serverName := endpoint.ServerName
		if serverName == "" {
			serverName = authorityHostname(authority)
		}
		serverName, err = normalizeServerName(serverName)
		if err != nil {
			return fmt.Errorf("endpoint %q server_name: %w", rawAuthority, err)
		}
		normalized[authority] = EndpointConfig{DialAddress: dialAddress, ServerName: serverName}
	}
	c.Endpoints = normalized
	return nil
}

func normalizeAuthority(raw string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return "", fmt.Errorf("must be a non-empty authority without surrounding whitespace")
	}
	if strings.Contains(raw, "://") {
		return "", fmt.Errorf("must omit the URL scheme")
	}
	u, err := url.Parse("mark://" + raw)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must contain only host and optional port")
	}
	host, _, err := fetch.ParseMarkURL(u.String())
	if err != nil {
		return "", err
	}
	return strings.ToLower(host), nil
}

func authorityHostname(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(authority, "[]")
}

func normalizeServerName(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" || name != raw {
		return "", fmt.Errorf("must be a non-empty lowercase DNS name without surrounding whitespace")
	}
	if strings.HasSuffix(name, ".") || net.ParseIP(name) != nil {
		return "", fmt.Errorf("must be a DNS name without a trailing dot")
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid DNS name %q", raw)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", fmt.Errorf("invalid DNS name %q", raw)
			}
		}
	}
	return name, nil
}
