package config

import (
	"os"
	"testing"

	"github.com/latebit/demarkus/protocol"
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

func TestNewConfig_MissingRoot(t *testing.T) {
	if err := os.Unsetenv("DEMARKUS_ROOT"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}

	cfg, err := NewConfig()
	if err == nil {
		t.Fatal("expected error for missing DEMARKUS_ROOT")
	}
	if cfg == nil {
		t.Fatal("expected config with defaults even when root is missing")
	}
	if cfg.Port != protocol.DefaultPort {
		t.Errorf("port: got %d, want default %d", cfg.Port, protocol.DefaultPort)
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

	cfg, err := NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != protocol.DefaultPort {
		t.Errorf("port: got %d, want default %d", cfg.Port, protocol.DefaultPort)
	}
}
