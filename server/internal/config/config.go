// Package config provides environment-based configuration for the Demarkus server.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// Config holds the server configuration.
type Config struct {
	Port           int
	ContentDir     string
	MaxStreams     int
	IdleTimeout    time.Duration // Timeout for idle connections
	RequestTimeout time.Duration // Timeout for handling a single request
	TLSCert        string        // Path to TLS certificate PEM file (empty = dev mode)
	TLSKey         string        // Path to TLS private key PEM file (empty = dev mode)
	TokensFile     string        // Path to TOML tokens file (empty = no auth)
	RateLimit      float64       // Requests per second per IP (0 = disabled)
	RateBurst      int           // Burst size for rate limiter
	LogFormat      string        // Log format: "text" (default) or "json"
	LogLevel       string        // Log level: "debug", "info" (default), "warn", "error"
	ReadOnly       bool          // Reject all write operations (PUBLISH, APPEND, ARCHIVE)
	StoreBackend   string        // Document store backend: "file" (default) or "postgres"
	PostgresDSN    string        // Postgres connection string (required for the postgres backend)
}

// NewConfig loads DEMARKUS_-prefixed environment configuration. Unparseable
// values are errors, not silent defaults. Semantic rules live in Validate,
// which the caller runs after CLI-flag overrides.
func NewConfig() (*Config, error) {
	config := &Config{}
	var errs []error

	config.Port = getEnvAsInt("DEMARKUS_PORT", protocol.DefaultPort, &errs)
	config.ContentDir = getEnv("DEMARKUS_ROOT", "")
	config.MaxStreams = getEnvAsInt("DEMARKUS_MAX_STREAMS", 10, &errs)
	config.IdleTimeout = getEnvAsDuration("DEMARKUS_IDLE_TIMEOUT", 30*time.Second, &errs)
	config.RequestTimeout = getEnvAsDuration("DEMARKUS_REQUEST_TIMEOUT", 10*time.Second, &errs)
	config.TLSCert = getEnv("DEMARKUS_TLS_CERT", "")
	config.TLSKey = getEnv("DEMARKUS_TLS_KEY", "")
	config.TokensFile = getEnv("DEMARKUS_TOKENS", "")
	config.RateLimit = getEnvAsFloat64("DEMARKUS_RATE_LIMIT", 50, &errs)
	config.RateBurst = getEnvAsInt("DEMARKUS_RATE_BURST", 100, &errs)
	config.LogFormat = getEnv("DEMARKUS_LOG_FORMAT", "text")
	config.LogLevel = getEnv("DEMARKUS_LOG_LEVEL", "info")
	config.ReadOnly = getEnvAsBool("DEMARKUS_READ_ONLY", false, &errs)
	config.StoreBackend = getEnv("DEMARKUS_STORE", "file")
	config.PostgresDSN = getEnv("DEMARKUS_PG_DSN", "")

	return config, errors.Join(errs...)
}

// Validate checks the semantic rules (rate limits, store backend) on the
// final values. Called once after CLI-flag overrides so every configuration
// path enforces one contract.
func (c *Config) Validate() error {
	var errs []error
	if math.IsNaN(c.RateLimit) || math.IsInf(c.RateLimit, 0) {
		errs = append(errs, fmt.Errorf("rate limit must be finite (got %v)", c.RateLimit))
	} else if c.RateLimit < 0 {
		errs = append(errs, fmt.Errorf("rate limit must be non-negative (got %v)", c.RateLimit))
	}
	if c.RateBurst < 0 {
		errs = append(errs, fmt.Errorf("rate burst must be non-negative (got %d)", c.RateBurst))
	}
	if c.RateLimit > 0 && c.RateBurst < 1 {
		errs = append(errs, fmt.Errorf("rate burst must be at least 1 when rate limiting is enabled (got %d)", c.RateBurst))
	}
	// Negative timeouts silently disable deadlines rather than expiring
	// requests; zero stays valid (documented no-deadline mode).
	if c.IdleTimeout < 0 {
		errs = append(errs, fmt.Errorf("idle timeout must be non-negative (got %v)", c.IdleTimeout))
	}
	if c.RequestTimeout < 0 {
		errs = append(errs, fmt.Errorf("request timeout must be non-negative (got %v)", c.RequestTimeout))
	}
	if err := c.ValidateStoreBackend(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidateStoreBackend checks the store-backend selection and its required
// settings on the final (post-override) values.
func (c *Config) ValidateStoreBackend() error {
	switch c.StoreBackend {
	case "", "file":
		c.StoreBackend = "file"
		if c.ContentDir == "" {
			return errors.New("content directory is required (set DEMARKUS_ROOT or use -root)")
		}
		// Validate content directory exists and is readable.
		info, err := os.Stat(c.ContentDir)
		if err != nil {
			return fmt.Errorf("content directory %q: %w", c.ContentDir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("content directory %q is not a directory", c.ContentDir)
		}
	case "postgres":
		if c.PostgresDSN == "" {
			return errors.New("postgres store requires a DSN (set DEMARKUS_PG_DSN or use -pg-dsn)")
		}
	default:
		return fmt.Errorf("store backend must be \"file\" or \"postgres\" (got %q)", c.StoreBackend)
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		return defaultValue
	}
	return value
}

func getEnvAsInt(key string, defaultValue int, errs *[]error) int {
	valueStr := getEnv(key, "")
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid integer %q", key, valueStr))
		return defaultValue
	}
	return value
}

func getEnvAsFloat64(key string, defaultValue float64, errs *[]error) float64 {
	valueStr := getEnv(key, "")
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid number %q", key, valueStr))
		return defaultValue
	}
	return value
}

func getEnvAsBool(key string, defaultValue bool, errs *[]error) bool {
	valueStr := getEnv(key, "")
	switch valueStr {
	case "":
		return defaultValue
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		*errs = append(*errs, fmt.Errorf("%s: invalid boolean %q (use 1/true/yes or 0/false/no)", key, valueStr))
		return defaultValue
	}
}

func getEnvAsDuration(key string, defaultValue time.Duration, errs *[]error) time.Duration {
	valueStr := getEnv(key, "")
	if valueStr == "" {
		return defaultValue
	}
	value, err := time.ParseDuration(valueStr)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid duration %q", key, valueStr))
		return defaultValue
	}
	return value
}
