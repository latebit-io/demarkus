package fedcrawl

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Crawl.MaxDepth != 2 {
		t.Errorf("MaxDepth = %d, want 2", cfg.Crawl.MaxDepth)
	}
	if cfg.Crawl.MaxServers != 50 {
		t.Errorf("MaxServers = %d, want 50", cfg.Crawl.MaxServers)
	}
	if cfg.Crawl.MaxDocuments != 1000 {
		t.Errorf("MaxDocuments = %d, want 1000", cfg.Crawl.MaxDocuments)
	}
	if cfg.Crawl.Workers != 5 {
		t.Errorf("Workers = %d, want 5", cfg.Crawl.Workers)
	}
	if cfg.Schedule.Interval != 6*time.Hour {
		t.Errorf("Interval = %v, want 6h", cfg.Schedule.Interval)
	}
	if cfg.Politeness.RequestDelay != 100*time.Millisecond {
		t.Errorf("RequestDelay = %v, want 100ms", cfg.Politeness.RequestDelay)
	}
	if cfg.Politeness.PerServerConcurrency != 2 {
		t.Errorf("PerServerConcurrency = %d, want 2", cfg.Politeness.PerServerConcurrency)
	}
}

func TestLoadNonexistent(t *testing.T) {
	cfg, err := Load("/nonexistent/config.toml", []string{"mark://example.com"})
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Seeds) != 1 || cfg.Seeds[0] != "mark://example.com" {
		t.Errorf("Seeds = %v, want [mark://example.com]", cfg.Seeds)
	}
}

func TestLoadEmptyPath(t *testing.T) {
	cfg, err := Load("", []string{"mark://example.com"})
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Seeds) != 1 || cfg.Seeds[0] != "mark://example.com" {
		t.Errorf("Seeds = %v, want [mark://example.com]", cfg.Seeds)
	}
}

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
seeds = ["mark://hub.example.com", "mark://peer.example.com"]
hubs = ["mark://hub.example.com"]

[crawl]
max_depth = 3
max_servers = 100
max_documents = 5000
workers = 10

[schedule]
interval = "12h"

[politeness]
request_delay = "200ms"
per_server_concurrency = 3
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Seeds) != 2 {
		t.Errorf("len(Seeds) = %d, want 2", len(cfg.Seeds))
	}
	if len(cfg.Hubs) != 1 {
		t.Errorf("len(Hubs) = %d, want 1", len(cfg.Hubs))
	}
	if cfg.Crawl.MaxDepth != 3 {
		t.Errorf("MaxDepth = %d, want 3", cfg.Crawl.MaxDepth)
	}
	if cfg.Crawl.MaxServers != 100 {
		t.Errorf("MaxServers = %d, want 100", cfg.Crawl.MaxServers)
	}
	if cfg.Crawl.MaxDocuments != 5000 {
		t.Errorf("MaxDocuments = %d, want 5000", cfg.Crawl.MaxDocuments)
	}
	if cfg.Crawl.Workers != 10 {
		t.Errorf("Workers = %d, want 10", cfg.Crawl.Workers)
	}
	if cfg.Schedule.Interval != 12*time.Hour {
		t.Errorf("Interval = %v, want 12h", cfg.Schedule.Interval)
	}
	if cfg.Politeness.RequestDelay != 200*time.Millisecond {
		t.Errorf("RequestDelay = %v, want 200ms", cfg.Politeness.RequestDelay)
	}
	if cfg.Politeness.PerServerConcurrency != 3 {
		t.Errorf("PerServerConcurrency = %d, want 3", cfg.Politeness.PerServerConcurrency)
	}
}

func TestLoadPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
seeds = ["mark://example.com"]

[crawl]
max_depth = 5
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Specified values.
	if cfg.Crawl.MaxDepth != 5 {
		t.Errorf("MaxDepth = %d, want 5", cfg.Crawl.MaxDepth)
	}

	// Defaults for unspecified.
	if cfg.Crawl.MaxServers != 50 {
		t.Errorf("MaxServers = %d, want 50 (default)", cfg.Crawl.MaxServers)
	}
	if cfg.Schedule.Interval != 6*time.Hour {
		t.Errorf("Interval = %v, want 6h (default)", cfg.Schedule.Interval)
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("not valid toml {{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

func TestValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://example.com"}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() error: %v", err)
		}
	})

	t.Run("no seeds", func(t *testing.T) {
		cfg := DefaultConfig()
		if err := cfg.Validate(); err == nil {
			t.Error("expected error for no seeds")
		}
	})

	t.Run("invalid seed scheme", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"https://example.com"}
		if err := cfg.Validate(); err == nil {
			t.Error("expected error for non-mark seed")
		}
	})

	t.Run("invalid hub scheme", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://example.com"}
		cfg.Hubs = []string{"https://example.com"}
		if err := cfg.Validate(); err == nil {
			t.Error("expected error for non-mark hub")
		}
	})

	t.Run("zero workers", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://example.com"}
		cfg.Crawl.Workers = 0
		if err := cfg.Validate(); err == nil {
			t.Error("expected error for zero workers")
		}
	})

	t.Run("negative max depth", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://example.com"}
		cfg.Crawl.MaxDepth = -1
		if err := cfg.Validate(); err == nil {
			t.Error("expected error for negative max depth")
		}
	})

	t.Run("negative request delay", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://example.com"}
		cfg.Politeness.RequestDelay = -1
		if err := cfg.Validate(); err == nil {
			t.Error("expected error for negative request delay")
		}
	})
}
