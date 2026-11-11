package knowledgeconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/server/knowledgeserver/worldsfragment"
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
	// bootstrap is read and ignored: older brokers' fragments carry it.
	if world.Name != "fritz-3a9f" {
		t.Errorf("fragment world = %+v, want name fritz-3a9f", world)
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
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "knowledge", "internal", "broker", "storage", "testdata", "worlds-fragment.golden.yaml"))
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
	// Every field the broker writes must arrive; a dropped one parses cleanly.
	w := &worlds[0]
	if w.Seed != SeedMemory || !w.Gated || w.Limits.MaxDocuments != 500 || len(w.Authorities) != 1 || w.Policy.Governance != GovernanceOpen {
		t.Fatalf("parsed fragment world = %+v", w)
	}
	// A tenant names no tokens file: the identity grant is its write path.
	if w.Auth.TokensFile != "" || w.Auth.StaticTokensFile != "" {
		t.Errorf("fragment auth = %+v, want no token files", w.Auth)
	}
	for field, got := range map[string]string{
		"name":       w.Name,
		"authority":  w.Authorities[0],
		"bucket.url": w.Bucket.URL,
	} {
		if !strings.Contains(got, "eve-adams-998d03fa") {
			t.Errorf("fragment %s = %q, want the tenant slug in it", field, got)
		}
	}
	if !ValidWorldID(w.Bucket.WorldID) {
		t.Errorf("broker-derived worldID %q is not canonical", w.Bucket.WorldID)
	}
}

func TestLoadWithFragmentStandsInForTheFile(t *testing.T) {
	dir := writeConfigDir(t, dynamicBase, "")
	config, err := LoadWithFragment(filepath.Join(dir, "config.yaml"), []byte(worldsFragment))
	if err != nil {
		t.Fatalf("LoadWithFragment: %v", err)
	}
	if len(config.Worlds) != 1 || config.Worlds[0].Name != "fritz-3a9f" {
		t.Fatalf("worlds = %+v, want the pushed fragment's world", config.Worlds)
	}
	// An empty push means no dynamic worlds, whatever the file says.
	dir = writeConfigDir(t, dynamicBase, worldsFragment)
	config, err = LoadWithFragment(filepath.Join(dir, "config.yaml"), nil)
	if err != nil || len(config.Worlds) != 0 {
		t.Fatalf("empty push: worlds = %d, err = %v; want none", len(config.Worlds), err)
	}
}

// Every field of the shared fragment type must arrive in the world the
// server opens: a field the consumer forgot would parse cleanly and vanish.
func TestGeneratedFragmentFieldsReachTheWorld(t *testing.T) {
	tenant := worldsfragment.World{
		Name: "eve-1", Authorities: []string{"eve-1.memory.svc.cluster.local"}, Gated: true, Seed: worldsfragment.SeedMemory,
		Bucket: worldsfragment.Bucket{URL: "gs://memory-eve-1", WorldID: "52b471f7-8d38-4c89-b44a-6f4f8b1a4f48"},
		Limits: &worldsfragment.Limits{MaxDocuments: 500},
		Policy: &worldsfragment.Policy{Governance: string(GovernanceOpen)},
	}
	data, err := worldsfragment.Render([]worldsfragment.World{tenant})
	if err != nil {
		t.Fatal(err)
	}
	dir := writeConfigDir(t, dynamicBase, string(data))
	config, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.FragmentWarning != "" {
		t.Errorf("warning on a fragment this build rendered: %q", config.FragmentWarning)
	}
	w := &config.Worlds[0]
	if w.Name != tenant.Name || !w.Gated || w.Bucket.URL != tenant.Bucket.URL || w.Bucket.WorldID != tenant.Bucket.WorldID ||
		w.Limits.MaxDocuments != 500 || w.Policy.Governance != GovernanceOpen || len(w.Authorities) != 1 || w.Seed != SeedMemory {
		t.Fatalf("world = %+v, want every fragment field", w)
	}
	// The defaults of any other world apply.
	if w.Limits.MaxConcurrentRequests != 32 || w.Policy.Path == "" {
		t.Errorf("fragment defaults missing: %+v", w)
	}
	// Reflection keeps the list above honest when the type grows.
	if got := reflect.TypeOf(tenant).NumField(); got != 7 {
		t.Fatalf("worldsfragment.World has %d fields; map the new one in worldFromFragment and assert it here", got)
	}
}

// A newer broker's key in a generated fragment must not stop the worlds
// from loading; an operator's file with the same key is still an error.
func TestGeneratedFragmentToleratesUnknownKeysOperatorFileDoesNot(t *testing.T) {
	body := `worlds:
  - name: eve-1
    authorities: [eve-1.memory.svc.cluster.local]
    bucket: {url: "gs://memory-eve-1", worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48}
    gated: true
    futureKnob: 3
`
	dir := writeConfigDir(t, dynamicBase, worldsfragment.Header+body)
	config, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("generated fragment with an unknown key: %v", err)
	}
	if len(config.Worlds) != 1 || !config.Worlds[0].Gated {
		t.Fatalf("worlds = %+v", config.Worlds)
	}
	if !strings.Contains(config.FragmentWarning, "futureKnob") {
		t.Errorf("FragmentWarning = %q, want the skipped key named", config.FragmentWarning)
	}

	dir = writeConfigDir(t, dynamicBase, body)
	if _, err := Load(filepath.Join(dir, "config.yaml")); err == nil || !strings.Contains(err.Error(), "futureKnob") {
		t.Fatalf("operator file with a mistyped key: err = %v, want it refused", err)
	}
}

// A seed kind this build cannot seed, from a newer broker, must not stall
// the fragment: the world takes the default seed and the log says so. In an
// operator's file the same kind is a mistake.
func TestUnknownSeedKindDefaultsInAGeneratedFragmentOnly(t *testing.T) {
	body := `worlds:
  - name: eve-1
    authorities: [eve-1.memory.svc.cluster.local]
    bucket: {url: "gs://memory-eve-1", worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48}
    gated: true
    seed: team
`
	dir := writeConfigDir(t, dynamicBase, worldsfragment.Header+body)
	config, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("generated fragment with a seed kind of a newer broker: %v", err)
	}
	if config.Worlds[0].Seed != "" || !strings.Contains(config.FragmentWarning, `seed "team"`) {
		t.Fatalf("seed = %q, warning = %q; want the default seed and the kind named", config.Worlds[0].Seed, config.FragmentWarning)
	}

	dir = writeConfigDir(t, dynamicBase, body)
	if _, err := Load(filepath.Join(dir, "config.yaml")); err == nil || !strings.Contains(err.Error(), `seed "team"`) {
		t.Fatalf("operator file with an unknown seed kind: err = %v, want it refused", err)
	}
}

// An operator's static memory world asks for the memory seed with the same
// key; it excludes the world's other seed sources.
func TestSeedKeyOnAnOperatorsWorld(t *testing.T) {
	world := `worlds:
  - name: alice
    authorities: [alice.memory.svc.cluster.local]
    bucket: {url: "gs://memory-alice", worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48}
    gated: true
    seed: memory
`
	dir := writeConfigDir(t, dynamicBase, world)
	config, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil || config.Worlds[0].Seed != SeedMemory {
		t.Fatalf("seed: memory on an operator's world: %v", err)
	}
	for extra, want := range map[string]string{
		"    policy: {file: /etc/demarkus/policy.md}\n": "must not be set with policy.file",
		"    policy: {optional: true}\n":                "must not be set on an optional policy",
		"    readOnly: true\n":                          "must not be set on a read-only world",
	} {
		dir := writeConfigDir(t, dynamicBase, world+extra)
		if _, err := Load(filepath.Join(dir, "config.yaml")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("seed with %q: err = %v, want %q", strings.TrimSpace(extra), err, want)
		}
	}
}
