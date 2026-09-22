package broker

import (
	"fmt"
	"time"
)

// defaultMCPAddr keeps the gateway off the management API's usual :8080.
const defaultMCPAddr = ":8081"

// First mint retry defaults: about 16s across 6 attempts, under the typical
// kubelet Secret propagation window without holding a tool call open longer.
const (
	defaultFirstMintMaxAttempts    = 6
	defaultFirstMintInitialBackoff = 250 * time.Millisecond
	defaultFirstMintMaxBackoff     = 8 * time.Second
)

// applyRefreshDefaults fills the refresh flow knobs and rejects degenerate
// values. Applied to the config itself so every consumer sees resolved values.
func (s *ServerConfig) applyRefreshDefaults() error {
	if s.RefreshTokensSecret == "" {
		s.RefreshTokensSecret = defaultRefreshTokensSecret
	}
	if s.DynamicClientsSecret == "" {
		s.DynamicClientsSecret = defaultDynamicClientsSecret
	}
	if s.RefreshTokenTTL == 0 {
		s.RefreshTokenTTL = defaultRefreshTokenTTL
	}
	if s.RefreshTokenTTL < 0 {
		return fmt.Errorf("server.refreshTokenTTL must be > 0 (got %s)", s.RefreshTokenTTL)
	}
	if s.IDTokenTTL == 0 {
		s.IDTokenTTL = defaultIDTokenTTL
	}
	if s.IDTokenTTL < 0 {
		return fmt.Errorf("server.idTokenTTL must be > 0 (got %s)", s.IDTokenTTL)
	}
	// A refresh credential that expires before the bearer it mints could
	// never renew it.
	if s.IDTokenTTL >= s.RefreshTokenTTL {
		return fmt.Errorf("server.idTokenTTL (%s) must be < server.refreshTokenTTL (%s); a bearer that outlives its refresh credential cannot be renewed", s.IDTokenTTL, s.RefreshTokenTTL)
	}
	return nil
}

// Device flow defaults: the TTL Google and Auth0 use, and the poll interval
// advertised to clients.
const (
	defaultDeviceCodeTTL      = 10 * time.Minute
	defaultDevicePollInterval = 5 * time.Second
)

// applyDeviceFlowDefaults fills the device flow knobs. The poll interval must
// stay under the TTL, or every legitimate poll would trip slow_down.
func (s *ServerConfig) applyDeviceFlowDefaults() error {
	if s.DeviceCodeTTL == 0 {
		s.DeviceCodeTTL = defaultDeviceCodeTTL
	}
	if s.DeviceCodeTTL < 0 {
		return fmt.Errorf("server.deviceCodeTTL must be > 0 (got %s)", s.DeviceCodeTTL)
	}
	if s.DevicePollInterval == 0 {
		s.DevicePollInterval = defaultDevicePollInterval
	}
	if s.DevicePollInterval < 0 {
		return fmt.Errorf("server.devicePollInterval must be > 0 (got %s)", s.DevicePollInterval)
	}
	if s.DevicePollInterval >= s.DeviceCodeTTL {
		return fmt.Errorf("server.devicePollInterval (%s) must be < server.deviceCodeTTL (%s)", s.DevicePollInterval, s.DeviceCodeTTL)
	}
	return nil
}

// applyDefaultsAndValidate fills the rate limit knobs and rejects negative
// values. A disabled limiter may leave the route blocks empty.
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
