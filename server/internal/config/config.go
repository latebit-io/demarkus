// Package config provides environment-based configuration for the Demarkus server.
package config

import (
	"errors"
	"fmt"
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

// NewConfig loads configuration from environment variables.
// Environment variables are prefixed with DEMARKUS_.
func NewConfig() (*Config, error) {
	config := &Config{}

	config.Port = getEnvAsInt("DEMARKUS_PORT", protocol.DefaultPort)
	config.ContentDir = getEnv("DEMARKUS_ROOT", "")
	config.MaxStreams = getEnvAsInt("DEMARKUS_MAX_STREAMS", 10)
	config.IdleTimeout = getEnvAsDuration("DEMARKUS_IDLE_TIMEOUT", 30*time.Second)
	config.RequestTimeout = getEnvAsDuration("DEMARKUS_REQUEST_TIMEOUT", 10*time.Second)
	config.TLSCert = getEnv("DEMARKUS_TLS_CERT", "")
	config.TLSKey = getEnv("DEMARKUS_TLS_KEY", "")
	config.TokensFile = getEnv("DEMARKUS_TOKENS", "")
	config.RateLimit = getEnvAsFloat64("DEMARKUS_RATE_LIMIT", 50)
	config.RateBurst = getEnvAsInt("DEMARKUS_RATE_BURST", 100)
	config.LogFormat = getEnv("DEMARKUS_LOG_FORMAT", "text")
	config.LogLevel = getEnv("DEMARKUS_LOG_LEVEL", "info")
	if v := getEnv("DEMARKUS_READ_ONLY", ""); v != "" {
		config.ReadOnly = v == "1" || v == "true" || v == "yes"
	}
	config.StoreBackend = getEnv("DEMARKUS_STORE", "file")
	config.PostgresDSN = getEnv("DEMARKUS_PG_DSN", "")

	if config.RateLimit < 0 {
		return config, fmt.Errorf("DEMARKUS_RATE_LIMIT must be non-negative (got %v)", config.RateLimit)
	}
	if config.RateBurst < 0 {
		return config, fmt.Errorf("DEMARKUS_RATE_BURST must be non-negative (got %d)", config.RateBurst)
	}
	if config.RateLimit > 0 && config.RateBurst < 1 {
		return config, fmt.Errorf("DEMARKUS_RATE_BURST must be at least 1 when rate limiting is enabled (got %d)", config.RateBurst)
	}

	if err := config.ValidateStoreBackend(); err != nil {
		return config, err
	}

	return config, nil
}

// ValidateStoreBackend checks the store-backend selection and its required
// settings. NewConfig calls it against the environment-derived values;
// main.go calls it again after CLI flag overrides so both configuration
// paths enforce one contract.
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

func getEnvAsInt(key string, defaultValue int) int {
	valueStr := getEnv(key, "")
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

func getEnvAsFloat64(key string, defaultValue float64) float64 {
	valueStr := getEnv(key, "")
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return defaultValue
	}
	return value
}

func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	valueStr := getEnv(key, "")
	if valueStr == "" {
		return defaultValue
	}
	value, err := time.ParseDuration(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}
