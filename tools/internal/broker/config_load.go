package broker

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadConfig reads, parses and validates a broker config file. Validation is
// eager so a misconfigured broker fails to start, not at the first login.
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

// applyEnvOverrides takes the two secrets from the environment when set, so
// they can come from a secretKeyRef instead of the rendered config. An empty
// variable is unset, so a cleared variable cannot blank a file value.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("OIDC_CLIENT_SECRET"); v != "" {
		c.OIDC.ClientSecret = v
	}
	if v := os.Getenv("BROKER_SIGNING_KEY"); v != "" {
		c.OIDC.BrokerSigningKey = v
	}
}
