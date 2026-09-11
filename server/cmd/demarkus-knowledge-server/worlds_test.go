package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/certsource"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
	"github.com/latebit-io/demarkus/server/internal/knowledge/knowledgeseed"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
)

// worldsTestHarness runs a worldManager over in-memory blob stores and a
// real on-disk config + worlds fragment, the same merged-Load path the
// production hot-reload uses.
type worldsTestHarness struct {
	dir     string
	manager *worldManager
	// storesMu guards stores: the manager's retry loop and the test
	// goroutine both drive newStore.
	storesMu sync.Mutex
	stores   map[string]*blob.Memory
}

const testWorldID = "52b471f7-8d38-4c89-b44a-6f4f8b1a4f48"
const testWorldIDB = "62b471f7-8d38-4c89-b44a-6f4f8b1a4f48"

func testCertFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certPEM, keyPEM := generateSelfSigned(t, "worlds.test")
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// worldFragment renders one world. Without bootstrap it is a statically
// configured world, the path that enforces policy and seeds a default one.
func worldFragment(name, worldID, tokensFile string, bootstrap bool) string {
	return fmt.Sprintf(`  - name: %s
    authorities:
      - %s.memory.svc.cluster.local
    bucket:
      url: gs://memory-%s
      worldID: %s
    auth:
      tokensFile: %s
    bootstrap: %t
`, name, name, name, worldID, tokensFile, bootstrap)
}

// worldPolicyFragment renders a statically configured world whose initial
// policy comes from a local file instead of the embedded default.
func worldPolicyFragment(name, worldID, tokensFile, policyFile string) string {
	return worldFragment(name, worldID, tokensFile, false) + fmt.Sprintf(`    policy:
      file: %s
`, policyFile)
}

func newWorldsHarness(t *testing.T, fragment string) *worldsTestHarness {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worlds.yaml"), []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := openHarness(t, dir, "worldsFile: worlds.yaml\n", nil)
	if err != nil {
		t.Fatalf("open harness: %v", err)
	}
	return h
}

// openHarness writes a config whose world set is worldsSection, either a
// worldsFile pointer or an inline worlds list, and opens a manager over it.
// newStore overrides the blob-store factory; nil uses in-memory buckets.
func openHarness(
	t *testing.T,
	dir, worldsSection string,
	newStore func(context.Context, *knowledgeconfig.WorldConfig) (blob.Store, error),
) (*worldsTestHarness, error) {
	t.Helper()
	certFile, keyFile := testCertFiles(t)
	main := fmt.Sprintf(`version: 1
tls:
  certFile: %s
  keyFile: %s
%s`, certFile, keyFile, worldsSection)
	configFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configFile, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := knowledgeconfig.Load(configFile)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	certs, err := certsource.Open(certFile, keyFile, nil)
	if err != nil {
		t.Fatalf("certsource: %v", err)
	}
	h := &worldsTestHarness{dir: dir, stores: map[string]*blob.Memory{}}
	if newStore == nil {
		newStore = func(_ context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error) {
			h.storesMu.Lock()
			defer h.storesMu.Unlock()
			if store, ok := h.stores[world.Name]; ok {
				return store, nil
			}
			store, err := blob.NewMemory(maxObjectBytes)
			if err != nil {
				return nil, err
			}
			h.stores[world.Name] = store
			return store, nil
		}
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	group := &sync.WaitGroup{}
	manager, err := newWorldManager(watchCtx, group, configFile, config, newStore, certs, slog.Default())
	if err != nil {
		cancel()
		group.Wait()
		// The harness carries the stores the failed open touched.
		return h, err
	}
	h.manager = manager
	t.Cleanup(func() {
		h.manager.Close()
		cancel()
		group.Wait()
	})
	return h, nil
}

func (h *worldsTestHarness) writeFragment(t *testing.T, fragment string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, "worlds.yaml"), []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTokens(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name+".toml")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (h *worldsTestHarness) objects(t *testing.T, world string) *blob.Memory {
	t.Helper()
	h.storesMu.Lock()
	defer h.storesMu.Unlock()
	store := h.stores[world]
	if store == nil {
		t.Fatalf("no store opened for %s", world)
	}
	return store
}

// seededPolicy reopens the world with enforcement on and returns its policy,
// which only exists if the manager seeded one.
func (h *worldsTestHarness) seededPolicy(t *testing.T, world string) *protocolstore.Document {
	t.Helper()
	store, err := bucketstore.Open(context.Background(), h.objects(t, world), bucketstore.Options{
		WorldID: testWorldID, RequirePolicy: true,
	})
	if err != nil {
		t.Fatalf("reopen seeded world: %v", err)
	}
	document, err := store.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		t.Fatalf("get seeded policy: %v", err)
	}
	return document
}

func (h *worldsTestHarness) routes(authority string) bool {
	router := h.manager.Router()
	// Route via the internal lookup the handshake hook uses.
	hook, err := router.HandshakeHook(tlsProbe)
	if err != nil {
		return false
	}
	_, err = hook(clientHello(authority))
	return err == nil
}

func TestWorldManagerHotAddAndRemove(t *testing.T) {
	tokensDir := t.TempDir()
	tokensA := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokensA, true))

	if got := h.manager.WorldCount(); got != 1 {
		t.Fatalf("initial worlds = %d, want 1", got)
	}
	if !h.routes("alice.memory.svc.cluster.local") {
		t.Fatal("initial world does not route")
	}

	// Hot add a second world through the fragment.
	tokensB := writeTokens(t, tokensDir, "bob")
	h.writeFragment(t, "worlds:\n"+
		worldFragment("alice", testWorldID, tokensA, true)+
		worldFragment("bob", testWorldIDB, tokensB, true))
	if err := h.manager.Reload(); err != nil {
		t.Fatalf("reload after add: %v", err)
	}
	if got := h.manager.WorldCount(); got != 2 {
		t.Fatalf("worlds after add = %d, want 2", got)
	}
	if !h.routes("bob.memory.svc.cluster.local") {
		t.Fatal("added world does not route")
	}

	// Remove the first world.
	h.writeFragment(t, "worlds:\n"+worldFragment("bob", testWorldIDB, tokensB, true))
	if err := h.manager.Reload(); err != nil {
		t.Fatalf("reload after remove: %v", err)
	}
	if got := h.manager.WorldCount(); got != 1 {
		t.Fatalf("worlds after remove = %d, want 1", got)
	}
	if h.routes("alice.memory.svc.cluster.local") {
		t.Fatal("removed world still routes")
	}
	if !h.routes("bob.memory.svc.cluster.local") {
		t.Fatal("remaining world lost routing")
	}
}

func TestWorldManagerBootstrapInitializesGenesis(t *testing.T) {
	tokensDir := t.TempDir()
	tokens := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, true))

	// The memory blob store started empty; a successful open proves the
	// bootstrap path wrote genesis. The head object must now exist.
	if _, err := h.objects(t, "alice").Get(context.Background(), "_demarkus/v1/head.json"); err != nil {
		t.Fatalf("bootstrap did not create genesis head: %v", err)
	}
}

func TestWorldManagerSeedsDefaultPolicyForStaticWorld(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "acme")
	h, err := openHarness(t, t.TempDir(), "worlds:\n"+worldFragment("acme", testWorldID, tokens, false), nil)
	if err != nil {
		t.Fatalf("static world with an empty bucket must come up: %v", err)
	}
	if got := h.manager.WorldCount(); got != 1 {
		t.Fatalf("worlds = %d, want 1", got)
	}

	// The world enforces policy, so the open could only succeed if it
	// created genesis and seeded the default policy itself.
	document := h.seededPolicy(t, "acme")
	if !bytes.Equal(document.Content, knowledgeseed.DefaultPolicySeed().Body) {
		t.Errorf("seeded policy = %q", document.Content)
	}
}

func TestWorldManagerSeedsConfiguredPolicyFile(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "acme")
	body := "# Write Policy\n\nOurs from the start.\n\nstrictness: block\nrequire_tags: category\n"
	policyFile := filepath.Join(t.TempDir(), "policy.md")
	if err := os.WriteFile(policyFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	worlds := "worlds:\n" + worldPolicyFragment("acme", testWorldID, tokens, policyFile)
	h, err := openHarness(t, t.TempDir(), worlds, nil)
	if err != nil {
		t.Fatalf("world with a configured policy file must come up: %v", err)
	}

	// Version 1: the operator's policy is the world's first, not a second
	// version published over the embedded default.
	document := h.seededPolicy(t, "acme")
	if string(document.Content) != body || document.Version != 1 {
		t.Errorf("seeded policy = version %d %q", document.Version, document.Content)
	}
	if bytes.Equal(document.Content, knowledgeseed.DefaultPolicySeed().Body) {
		t.Error("configured policy file did not override the embedded default")
	}
}

func TestWorldManagerRefusesUnreadablePolicyFile(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "acme")
	missing := filepath.Join(t.TempDir(), "absent.md")
	worlds := "worlds:\n" + worldPolicyFragment("acme", testWorldID, tokens, missing)
	h, err := openHarness(t, t.TempDir(), worlds, nil)
	if err == nil {
		t.Fatal("manager started with an unreadable policy file")
	}
	// The seed is read before genesis, so a bad file leaves no world behind.
	listed, err := h.objects(t, "acme").List(context.Background(), "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed.Objects) != 0 {
		t.Errorf("objects created = %d, want none", len(listed.Objects))
	}
}

func TestWorldManagerRefusesBucketWithForeignObjects(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "acme")
	foreign := func(context.Context, *knowledgeconfig.WorldConfig) (blob.Store, error) {
		store, err := blob.NewMemory(maxObjectBytes)
		if err != nil {
			return nil, err
		}
		_, err = store.Create(context.Background(), "someone-elses-data.json", []byte("{}"))
		return store, err
	}
	// A typo'd bucket URL must fail the open, not silently become a new
	// empty world that reports healthy.
	_, err := openHarness(t, t.TempDir(), "worlds:\n"+worldFragment("acme", testWorldID, tokens, false), foreign)
	if err == nil {
		t.Fatal("manager started over a bucket holding foreign objects")
	}
	if !strings.Contains(err.Error(), "no world head") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestWorldManagerPendingRetryOnMissingTokens(t *testing.T) {
	tokensDir := t.TempDir()
	tokensA := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokensA, true))

	// A world whose tokens file has not propagated yet: open fails,
	// world parks in pending, others stay live.
	missing := filepath.Join(tokensDir, "carol.toml")
	h.writeFragment(t, "worlds:\n"+
		worldFragment("alice", testWorldID, tokensA, true)+
		worldFragment("carol", testWorldIDB, missing, true))
	if err := h.manager.Reload(); err != nil {
		t.Fatalf("reload with missing tokens: %v (resilient mode must not fail)", err)
	}
	if got := h.manager.WorldCount(); got != 1 {
		t.Fatalf("worlds = %d, want 1 (carol pending)", got)
	}

	// Tokens file appears; a retry pass brings the world up.
	if err := os.WriteFile(missing, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	h.manager.retryPending()
	if got := h.manager.WorldCount(); got != 2 {
		t.Fatalf("worlds after retry = %d, want 2", got)
	}
	if !h.routes("carol.memory.svc.cluster.local") {
		t.Fatal("recovered world does not route")
	}
}

func TestWorldManagerRejectsBadFragmentKeepsRunning(t *testing.T) {
	tokensDir := t.TempDir()
	tokensA := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokensA, true))

	h.writeFragment(t, "worlds:\n  - name: broken\n    nonsense: true\n")
	if err := h.manager.Reload(); err == nil {
		t.Fatal("reload of invalid fragment must surface an error")
	}
	if got := h.manager.WorldCount(); got != 1 {
		t.Fatalf("worlds after bad fragment = %d, want previous set kept", got)
	}
	if !h.routes("alice.memory.svc.cluster.local") {
		t.Fatal("previous world lost routing after rejected fragment")
	}
}

func TestWorldManagerStaticModeFailsFast(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "alice")
	// An unreachable bucket. An empty one is no longer a failure: the
	// server creates that world and seeds its policy.
	unreachable := func(context.Context, *knowledgeconfig.WorldConfig) (blob.Store, error) {
		return nil, errors.New("bucket unreachable")
	}
	worlds := "worlds:\n" + worldFragment("alice", testWorldID, tokens, false)
	if _, err := openHarness(t, t.TempDir(), worlds, unreachable); err == nil {
		t.Fatal("static mode must fail fast on an unopenable world")
	}
}
