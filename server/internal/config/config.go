// Package config provides environment-based configuration for the Demarkus server.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/latebit/demarkus/protocol"
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

	if config.ContentDir == "" {
		return config, errors.New("DEMARKUS_ROOT environment variable is required")
	}

	// Validate content directory exists and is readable.
	info, err := os.Stat(config.ContentDir)
	if err != nil {
		return config, fmt.Errorf("content directory %q: %w", config.ContentDir, err)
	}
	if !info.IsDir() {
		return config, fmt.Errorf("content directory %q is not a directory", config.ContentDir)
	}

	return config, nil
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
