// Package fedcrawl provides multi-server federation crawling for the Mark Protocol.
// It discovers servers, collects content hashes, and publishes indexes to hubs.
package fedcrawl

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config holds the crawler configuration loaded from a TOML file.
type Config struct {
	Seeds []string `toml:"seeds"` // Initial servers to crawl (mark://host:port)
	Hubs  []string `toml:"hubs"`  // Servers to publish indexes to

	Crawl      CrawlConfig      `toml:"crawl"`
	Schedule   ScheduleConfig   `toml:"schedule"`
	Politeness PolitenessConfig `toml:"politeness"`
	Publish    PublishConfig    `toml:"publish"`
}

// CrawlConfig controls the scope and depth of crawling.
type CrawlConfig struct {
	MaxDepth     int `toml:"max_depth"`     // Max link hops per server (default: 2)
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
			MaxDepth:     2,
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
	for i, seed := range c.Seeds {
		if !strings.HasPrefix(seed, "mark://") {
			return fmt.Errorf("seed %d %q must be a mark:// URL", i, seed)
		}
	}
	for i, hub := range c.Hubs {
		if !strings.HasPrefix(hub, "mark://") {
			return fmt.Errorf("hub %d %q must be a mark:// URL", i, hub)
		}
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
