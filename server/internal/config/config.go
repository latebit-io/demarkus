// Package config provides environment-based configuration for the Demarkus server.
package config

import (
	"errors"
	"os"
	"strconv"

	"github.com/latebit/demarkus/protocol"
)

// Config holds the server configuration.
type Config struct {
	Port       int
	ContentDir string
	MaxStreams int
}

// NewConfig loads configuration from environment variables.
// Environment variables are prefixed with DEMARKUS_.
func NewConfig() (*Config, error) {
	config := &Config{}

	config.Port = getEnvAsInt("DEMARKUS_PORT", protocol.DefaultPort)
	config.ContentDir = getEnv("DEMARKUS_ROOT", "")
	config.MaxStreams = getEnvAsInt("DEMARKUS_MAX_STREAMS", 10)

	if config.ContentDir == "" {
		return config, errors.New("DEMARKUS_ROOT environment variable is required")
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
