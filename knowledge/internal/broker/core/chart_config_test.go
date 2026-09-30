package core

import (
	"path/filepath"
	"testing"
)

// The chart's secret-broker-config.yaml renders this fixture (helm template
// with the chart's test fixture values); a key the chart emits that the
// loader no longer knows fails here, not at pod start.
func TestLoadConfigAcceptsTheChartRendering(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := LoadConfig(filepath.Join("testdata", "chart-rendered-config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig(chart rendering): %v", err)
	}
	if len(cfg.Worlds) != 2 || !cfg.Worlds[0].Local || cfg.Worlds[0].Profile != ProfileKnowledge {
		t.Fatalf("worlds = %+v, want two local knowledge worlds", cfg.Worlds)
	}
	if got := cfg.Worlds[0].InternalAddress; got != "team-a.example.com:6309" {
		t.Fatalf("internalAddress = %q, want the first authority", got)
	}
	if got := cfg.Server.Gateway(ProfileKnowledge).PublicURL; got != cfg.Server.PublicURL {
		t.Fatalf("knowledge gateway URL = %q, want the issuer", got)
	}
	if got := cfg.Server.BearerAddr; got != ":8443" {
		t.Fatalf("bearerAddr = %q, want the chart's bearer port", got)
	}
}
