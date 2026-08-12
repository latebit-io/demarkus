package config

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func TestNewConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)

	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != protocol.DefaultPort {
		t.Errorf("port: got %d, want %d", cfg.Port, protocol.DefaultPort)
	}
	if cfg.ContentDir != dir {
		t.Errorf("content dir: got %q, want %q", cfg.ContentDir, dir)
	}
	if cfg.MaxStreams != 10 {
		t.Errorf("max streams: got %d, want %d", cfg.MaxStreams, 10)
	}
	if cfg.RateLimit != 50 {
		t.Errorf("rate limit: got %v, want %v", cfg.RateLimit, 50.0)
	}
	if cfg.RateBurst != 100 {
		t.Errorf("rate burst: got %d, want %d", cfg.RateBurst, 100)
	}
}

func TestNewConfig_EnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)
	t.Setenv("DEMARKUS_PORT", "9000")
	t.Setenv("DEMARKUS_MAX_STREAMS", "50")

	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != 9000 {
		t.Errorf("port: got %d, want %d", cfg.Port, 9000)
	}
	if cfg.ContentDir != dir {
		t.Errorf("content dir: got %q, want %q", cfg.ContentDir, dir)
	}
	if cfg.MaxStreams != 50 {
		t.Errorf("max streams: got %d, want %d", cfg.MaxStreams, 50)
	}
}

func TestNewConfig_RateLimitOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)
	t.Setenv("DEMARKUS_RATE_LIMIT", "200.5")
	t.Setenv("DEMARKUS_RATE_BURST", "500")

	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RateLimit != 200.5 {
		t.Errorf("rate limit: got %v, want %v", cfg.RateLimit, 200.5)
	}
	if cfg.RateBurst != 500 {
		t.Errorf("rate burst: got %d, want %d", cfg.RateBurst, 500)
	}
}

func TestNewConfig_RateLimitDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)
	t.Setenv("DEMARKUS_RATE_LIMIT", "0")

	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RateLimit != 0 {
		t.Errorf("rate limit: got %v, want %v", cfg.RateLimit, 0.0)
	}
}

func TestValidate_RateRules(t *testing.T) {
	tests := []struct {
		name      string
		rateLimit float64
		rateBurst int
		wantErr   bool
	}{
		{"defaults valid", 50, 100, false},
		{"disabled valid", 0, 0, false},
		{"zero burst with limit enabled", 50, 0, true},
		{"negative rate limit", -10, 100, true},
		{"negative rate burst", 50, -5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				ContentDir: t.TempDir(),
				RateLimit:  tt.rateLimit,
				RateBurst:  tt.rateBurst,
			}
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewConfig_InvalidRateLimit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)
	t.Setenv("DEMARKUS_RATE_LIMIT", "not-a-number")

	_, err := NewConfig()
	if err == nil {
		t.Fatal("expected error for unparseable DEMARKUS_RATE_LIMIT")
	}
}

func TestNewConfig_MissingRoot(t *testing.T) {
	if err := os.Unsetenv("DEMARKUS_ROOT"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}

	// NewConfig defers store-backend checks so CLI flags can complete them;
	// ValidateStoreBackend is where a missing root must fail.
	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != protocol.DefaultPort {
		t.Errorf("port: got %d, want default %d", cfg.Port, protocol.DefaultPort)
	}
	if err := cfg.ValidateStoreBackend(); err == nil {
		t.Fatal("expected ValidateStoreBackend error for missing DEMARKUS_ROOT")
	}
}

func TestNewConfig_TokensFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)
	t.Setenv("DEMARKUS_TOKENS", "/path/to/tokens.toml")

	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TokensFile != "/path/to/tokens.toml" {
		t.Errorf("tokens file: got %q, want %q", cfg.TokensFile, "/path/to/tokens.toml")
	}
}

func TestNewConfig_InvalidInt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)
	t.Setenv("DEMARKUS_PORT", "not-a-number")

	_, err := NewConfig()
	if err == nil {
		t.Fatal("expected error for unparseable DEMARKUS_PORT")
	}
}

func TestNewConfig_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)
	t.Setenv("DEMARKUS_IDLE_TIMEOUT", "30")

	_, err := NewConfig()
	if err == nil {
		t.Fatal("expected error for unparseable DEMARKUS_IDLE_TIMEOUT")
	}
}

func TestNewConfig_ReadOnly(t *testing.T) {
	for _, val := range []string{"1", "true", "yes"} {
		t.Run("enabled with "+val, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DEMARKUS_ROOT", dir)
			t.Setenv("DEMARKUS_READ_ONLY", val)

			cfg, err := NewConfig()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !cfg.ReadOnly {
				t.Errorf("expected ReadOnly to be true for %q", val)
			}
		})
	}

	for _, val := range []string{"0", "false", "no"} {
		t.Run("disabled with "+val, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DEMARKUS_ROOT", dir)
			t.Setenv("DEMARKUS_READ_ONLY", val)

			cfg, err := NewConfig()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ReadOnly {
				t.Errorf("expected ReadOnly to be false for %q", val)
			}
		})
	}
}

func TestNewConfig_ReadOnlyDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEMARKUS_ROOT", dir)

	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ReadOnly {
		t.Error("expected ReadOnly to be false by default")
	}
}

func TestNewConfig_InvalidReadOnly(t *testing.T) {
	for _, val := range []string{"tru", "yess", "2", "on"} {
		t.Run(val, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DEMARKUS_ROOT", dir)
			t.Setenv("DEMARKUS_READ_ONLY", val)

			_, err := NewConfig()
			if err == nil {
				t.Fatalf("expected error for DEMARKUS_READ_ONLY=%q", val)
			}
		})
	}
}

func TestValidate_NonFiniteRateLimit(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			cfg := &Config{ContentDir: t.TempDir(), RateLimit: v, RateBurst: 100}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for rate limit %v", v)
			}
		})
	}
}

func TestValidate_NegativeTimeouts(t *testing.T) {
	for _, tt := range []struct {
		name string
		mut  func(*Config)
	}{
		{"negative idle timeout", func(c *Config) { c.IdleTimeout = -time.Second }},
		{"negative request timeout", func(c *Config) { c.RequestTimeout = -time.Second }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{ContentDir: t.TempDir(), RateLimit: 50, RateBurst: 100}
			tt.mut(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
