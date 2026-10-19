package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
)

// TestServesUpgradeCapture opens the soul a released plugin wrote
// (testdata/upgrade, scripts/capture-upgrade-fixture.sh) the way main does:
// reads, history and the old plugin's token work with no user step.
func TestServesUpgradeCapture(t *testing.T) {
	world, token := openUpgradeCapture(t, filepath.Join("..", "..", "..", "testdata", "upgrade", "home"))
	send := func(verb, path string, meta map[string]string, body string) protocol.Response {
		return exchange(t, world, protocol.Request{Verb: verb, Path: path, Metadata: meta, Body: body})
	}

	if r := send(protocol.VerbFetch, "/fixture/index.md", nil, ""); r.Status != protocol.StatusOK || r.Metadata["type"] != "Hub" {
		t.Errorf("hub: status %s, type %q", r.Status, r.Metadata["type"])
	}
	if r := send(protocol.VerbFetch, "/fixture/adr/0001-decision.md/v1", nil, ""); !strings.Contains(r.Body, "We keep markdown.") {
		t.Errorf("v1 of the ADR: status %s, body %q", r.Status, r.Body)
	}
	if r := send(protocol.VerbFetch, "/fixture/old.md", nil, ""); r.Status != protocol.StatusArchived {
		t.Errorf("archived note: status %s", r.Status)
	}
	if r := send(protocol.VerbLookup, "/", map[string]string{"query": "decision"}, ""); !strings.Contains(r.Body, "/fixture/adr/0001-decision.md") {
		t.Errorf("lookup: status %s, body %q", r.Status, r.Body)
	}
	if r := send(protocol.VerbList, "/fixture/journal/", nil, ""); r.Status != protocol.StatusOK || !strings.Contains(r.Body, ".md") {
		t.Errorf("journal listing: status %s, body %q", r.Status, r.Body)
	}

	meta := map[string]string{"auth": token, "expected-version": "2", "type": "ADR", "tags": "fixture,decision"}
	if r := send(protocol.VerbPublish, "/fixture/adr/0001-decision.md", meta, "# Decision\n\nStill markdown.\n"); r.Status != protocol.StatusCreated {
		t.Fatalf("publish with the old plugin token: status %s, body %q", r.Status, r.Body)
	}
	r := send(protocol.VerbVersions, "/fixture/adr/0001-decision.md", nil, "")
	if r.Metadata["chain-valid"] != "true" || r.Metadata["total"] != "3" {
		t.Errorf("history after a new write: chain-valid %q, total %q", r.Metadata["chain-valid"], r.Metadata["total"])
	}
}

// exchange sends one request in process and fails the test on a transport error.
func exchange(t *testing.T, world worldruntime.Exchanger, req protocol.Request) protocol.Response {
	t.Helper()
	resp, err := world.Exchange(context.Background(), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Verb, req.Path, err)
	}
	return resp
}

// openUpgradeCapture serves a copy of the capture's soul with its tokens
// file through main's runtime wiring, and returns the plugin's raw token.
func openUpgradeCapture(t *testing.T, home string) (world worldruntime.Exchanger, token string) {
	t.Helper()
	state := filepath.Join(home, ".demarkus")
	tokensFiles, err := filepath.Glob(filepath.Join(state, "tokens", "*", "tokens.toml"))
	if err != nil || len(tokensFiles) != 1 {
		t.Fatalf("tokens.toml in the capture: %v, %v", tokensFiles, err)
	}
	raw, err := os.ReadFile(filepath.Join(state, "plugin-memory.token"))
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ContentDir = t.TempDir()
	cfg.TokensFile = tokensFiles[0]
	if err := os.CopyFS(cfg.ContentDir, os.DirFS(filepath.Join(state, "soul"))); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	store, err := openStore(cfg, logger)
	if err != nil {
		t.Fatalf("open the captured soul: %v", err)
	}
	runtime, err := worldruntime.New(runtimeConfig(cfg, store, logger))
	if err != nil {
		t.Fatalf("world runtime over the captured soul: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})
	world, ok := runtime.Endpoint("").(worldruntime.Exchanger)
	if !ok {
		t.Fatal("the runtime endpoint does not exchange in process")
	}
	return world, strings.TrimSpace(string(raw))
}
