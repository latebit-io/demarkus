package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/config"
)

// A soul whose configured tokens file is missing does not start: opened
// without it, every path a read token would protect is public.
func TestSoulRefusesAMissingTokensFile(t *testing.T) {
	cfg, err := config.NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ContentDir = t.TempDir()
	cfg.TokensFile = filepath.Join(t.TempDir(), "typo.toml")
	cfg.Port = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server, err := open(cfg, slog.New(slog.DiscardHandler))
	if err == nil {
		server.Close()
		t.Fatal("soul opened with a missing tokens file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("open error = %v, want the missing file", err)
	}
}
