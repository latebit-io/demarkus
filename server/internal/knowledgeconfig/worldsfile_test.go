package knowledgeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dynamicBase = `version: 1
tls:
  certFile: cert.pem
  keyFile: key.pem
worldsFile: worlds.yaml
`

const worldsFragment = `worlds:
  - name: fritz-3a9f
    authorities:
      - fritz-3a9f.memory.svc.cluster.local
    bucket:
      url: gs://memory-fritz-3a9f
      worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48
    auth:
      tokensFile: /run/demarkus/worlds-tokens/fritz-3a9f.toml
    bootstrap: true
`

func writeConfigDir(t *testing.T, main, fragment string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if fragment != "" {
		if err := os.WriteFile(filepath.Join(dir, "worlds.yaml"), []byte(fragment), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadMergesWorldsFile(t *testing.T) {
	dir := writeConfigDir(t, dynamicBase, worldsFragment)
	config, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(config.Worlds) != 1 {
		t.Fatalf("worlds = %d, want 1 from fragment", len(config.Worlds))
	}
	world := &config.Worlds[0]
	if world.Name != "fritz-3a9f" || !world.Bootstrap {
		t.Errorf("fragment world = %+v, want name fritz-3a9f with bootstrap", world)
	}
	// Fragment worlds get the same defaults Parse applies.
	if world.Limits.MaxConcurrentRequests != 32 || world.Policy.Path == "" {
		t.Errorf("fragment defaults missing: %+v", world)
	}
}

func TestLoadToleratesMissingWorldsFile(t *testing.T) {
	dir := writeConfigDir(t, dynamicBase, "")
	config, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load with absent fragment: %v", err)
	}
	if len(config.Worlds) != 0 {
		t.Fatalf("worlds = %d, want 0 (provisioner has not written yet)", len(config.Worlds))
	}
}

func TestLoadValidatesAcrossMergedWorlds(t *testing.T) {
	// A fragment world duplicating a static world's NAME must fail the
	// merged validation; every other field is unique so the name check
	// is what trips.
	main := strings.Replace(validConfig, "version: 1", "version: 1\nworldsFile: worlds.yaml", 1)
	fragment := strings.Replace(worldsFragment, "name: fritz-3a9f", "name: world-a", 1)
	fragment = strings.Replace(fragment, "52b471f7-8d38-4c89-b44a-6f4f8b1a4f48", "62b471f7-8d38-4c89-b44a-6f4f8b1a4f48", 1)
	dir := writeConfigDir(t, main, fragment)
	_, err := Load(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), `name "world-a" duplicates`) {
		t.Fatalf("Load err = %v, want duplicate-name failure across merged worlds", err)
	}
}

func TestValidateRequiresWorldsWithoutWorldsFile(t *testing.T) {
	_, err := Parse([]byte(`version: 1
tls:
  certFile: cert.pem
  keyFile: key.pem
`))
	if err == nil || !strings.Contains(err.Error(), "at least one world") {
		t.Fatalf("Parse err = %v, want at-least-one-world failure without worldsFile", err)
	}
}

func TestParseWorldsFragmentEmpty(t *testing.T) {
	for _, input := range []string{"", "   \n", "# no tenants provisioned yet\n"} {
		worlds, err := ParseWorldsFragment([]byte(input))
		if err != nil || len(worlds) != 0 {
			t.Fatalf("ParseWorldsFragment(%q) = %v, %v; want empty, nil", input, worlds, err)
		}
	}
}

func TestParseWorldsFragmentRejectsUnknownFields(t *testing.T) {
	_, err := ParseWorldsFragment([]byte("worlds: []\nlisten:\n  address: \":1\"\n"))
	if err == nil {
		t.Fatal("fragment with non-worlds keys must be rejected (the provisioner owns only worlds)")
	}
}

// TestParsesBrokerRenderedFragment is the consumer side of the
// contract: the broker's TestWorldsFragmentGolden renders this fixture
// and the server must parse it (hand-written lookalikes drift silently).
func TestParsesBrokerRenderedFragment(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "tools", "internal", "broker", "testdata", "worlds-fragment.golden.yaml"))
	if err != nil {
		t.Fatalf("read broker golden fixture: %v", err)
	}
	worlds, err := ParseWorldsFragment(data)
	if err != nil {
		t.Fatalf("ParseWorldsFragment(broker golden): %v", err)
	}
	if len(worlds) != 1 {
		t.Fatalf("worlds = %d, want 1", len(worlds))
	}
	w := &worlds[0]
	if !w.Bootstrap || w.Limits.MaxDocuments != 500 || len(w.Authorities) != 1 {
		t.Errorf("parsed fragment world = %+v", w)
	}
	if !ValidWorldID(w.Bucket.WorldID) {
		t.Errorf("broker-derived worldID %q is not canonical", w.Bucket.WorldID)
	}
}
