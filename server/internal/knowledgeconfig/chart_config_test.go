package knowledgeconfig

import (
	"path/filepath"
	"testing"
	"time"
)

// scripts/check-chart-fixtures.sh renders this fixture from the chart, and CI
// checks it is current; a key the chart emits that the parser no longer
// accepts fails here, not at pod start.
func TestLoadAcceptsTheChartRendering(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "chart-rendered-config.yaml"))
	if err != nil {
		t.Fatalf("Load(chart rendering): %v", err)
	}
	if cfg.Listen.Address != ":6309" || cfg.Health.Address != ":8081" || cfg.Peers.Listen != ":6310" {
		t.Fatalf("addresses = %+v, %+v, %+v; want the chart's ports", cfg.Listen, cfg.Health, cfg.Peers)
	}
	if cfg.FloorWorld != "team-a" || cfg.ContentCheck.PII != "block" {
		t.Fatalf("floor = %q, pii = %q; want team-a and block", cfg.FloorWorld, cfg.ContentCheck.PII)
	}
	if len(cfg.Worlds) != 2 {
		t.Fatalf("worlds = %d, want 2", len(cfg.Worlds))
	}
	a, b := cfg.Worlds[0], cfg.Worlds[1]
	if a.Name != "team-a" || len(a.Authorities) != 2 || a.ReadOnly {
		t.Fatalf("team-a = %+v", a)
	}
	if a.Auth.TokensFile != "/run/demarkus/worlds/team-a/tokens.toml" || a.Auth.StaticTokensFile == "" {
		t.Fatalf("team-a auth = %+v, want the chart's mounted token files", a.Auth)
	}
	if !b.ReadOnly || b.Limits.MaxConcurrentRequests != 16 || time.Duration(b.Limits.RequestTimeout) != 5*time.Second {
		t.Fatalf("team-b = %+v, want read-only with the fixture's limits", b)
	}
}
