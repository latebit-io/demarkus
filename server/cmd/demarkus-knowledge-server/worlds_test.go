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

func fragmentWorld(name, worldID, tokensFile string) string {
	return fmt.Sprintf(`  - name: %s
    authorities:
      - %s.memory.svc.cluster.local
    bucket:
      url: gs://memory-%s
      worldID: %s
    auth:
      tokensFile: %s
    bootstrap: true
`, name, name, name, worldID, tokensFile)
}

// staticWorld is fragmentWorld without bootstrap: a statically configured
// world, the path that enforces policy and seeds a default one.
func staticWorld(name, worldID, tokensFile string) string {
	return fmt.Sprintf(`  - name: %s
    authorities:
      - %s.knowledge.svc.cluster.local
    bucket:
      url: gs://knowledge-%s
      worldID: %s
    auth:
      tokensFile: %s
`, name, name, name, worldID, tokensFile)
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

// newStaticWorldsHarness runs a manager over statically configured worlds.
// prepare, when set, runs against each world's fresh blob store before the
// manager opens it, so a test can stage bucket contents.
func newStaticWorldsHarness(t *testing.T, worlds string, prepare func(*testing.T, *blob.Memory)) (*worldsTestHarness, error) {
	t.Helper()
	return openHarness(t, t.TempDir(), "worlds:\n"+worlds, prepare)
}

// openHarness writes a config whose world set is worldsSection, either a
// worldsFile pointer or an inline worlds list, and opens a manager over it.
func openHarness(t *testing.T, dir, worldsSection string, prepare func(*testing.T, *blob.Memory)) (*worldsTestHarness, error) {
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
	newStore := func(_ context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error) {
		h.storesMu.Lock()
		defer h.storesMu.Unlock()
		if store, ok := h.stores[world.Name]; ok {
			return store, nil
		}
		store, err := blob.NewMemory(maxObjectBytes)
		if err != nil {
			return nil, err
		}
		if prepare != nil {
			prepare(t, store)
		}
		h.stores[world.Name] = store
		return store, nil
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	group := &sync.WaitGroup{}
	manager, err := newWorldManager(watchCtx, group, configFile, config, newStore, certs, slog.Default())
	if err != nil {
		cancel()
		group.Wait()
		return nil, err
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
	h := newWorldsHarness(t, "worlds:\n"+fragmentWorld("alice", testWorldID, tokensA))

	if got := h.manager.WorldCount(); got != 1 {
		t.Fatalf("initial worlds = %d, want 1", got)
	}
	if !h.routes("alice.memory.svc.cluster.local") {
		t.Fatal("initial world does not route")
	}

	// Hot add a second world through the fragment.
	tokensB := writeTokens(t, tokensDir, "bob")
	h.writeFragment(t, "worlds:\n"+
		fragmentWorld("alice", testWorldID, tokensA)+
		fragmentWorld("bob", testWorldIDB, tokensB))
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
	h.writeFragment(t, "worlds:\n"+fragmentWorld("bob", testWorldIDB, tokensB))
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
	h := newWorldsHarness(t, "worlds:\n"+fragmentWorld("alice", testWorldID, tokens))

	// The memory blob store started empty; a successful open proves the
	// bootstrap path wrote genesis. The head object must now exist.
	h.storesMu.Lock()
	store := h.stores["alice"]
	h.storesMu.Unlock()
	if store == nil {
		t.Fatal("no store opened for alice")
	}
	if _, err := store.Get(context.Background(), "_demarkus/v1/head.json"); err != nil {
		t.Fatalf("bootstrap did not create genesis head: %v", err)
	}
}

func TestWorldManagerSeedsDefaultPolicyForStaticWorld(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "acme")
	h, err := newStaticWorldsHarness(t, staticWorld("acme", testWorldID, tokens), nil)
	if err != nil {
		t.Fatalf("static world with an empty bucket must come up: %v", err)
	}
	if got := h.manager.WorldCount(); got != 1 {
		t.Fatalf("worlds = %d, want 1", got)
	}

	// The world enforces policy, so the open could only succeed if it
	// created genesis and seeded the default policy itself.
	h.storesMu.Lock()
	objects := h.stores["acme"]
	h.storesMu.Unlock()
	store, err := bucketstore.Open(context.Background(), objects, bucketstore.Options{
		WorldID: testWorldID, RequirePolicy: true,
	})
	if err != nil {
		t.Fatalf("reopen seeded world: %v", err)
	}
	document, err := store.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		t.Fatalf("get seeded policy: %v", err)
	}
	if !bytes.Equal(document.Content, knowledgeseed.PolicyBody()) {
		t.Errorf("seeded policy = %q", document.Content)
	}
}

func TestWorldManagerRefusesBucketWithForeignObjects(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "acme")
	stage := func(t *testing.T, store *blob.Memory) {
		t.Helper()
		if _, err := store.Create(context.Background(), "someone-elses-data.json", []byte("{}")); err != nil {
			t.Fatalf("stage foreign object: %v", err)
		}
	}
	// A typo'd bucket URL must fail the open, not silently become a new
	// empty world that reports healthy.
	_, err := newStaticWorldsHarness(t, staticWorld("acme", testWorldID, tokens), stage)
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
	h := newWorldsHarness(t, "worlds:\n"+fragmentWorld("alice", testWorldID, tokensA))

	// A world whose tokens file has not propagated yet: open fails,
	// world parks in pending, others stay live.
	missing := filepath.Join(tokensDir, "carol.toml")
	h.writeFragment(t, "worlds:\n"+
		fragmentWorld("alice", testWorldID, tokensA)+
		fragmentWorld("carol", testWorldIDB, missing))
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
	h := newWorldsHarness(t, "worlds:\n"+fragmentWorld("alice", testWorldID, tokensA))

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
	dir := t.TempDir()
	certFile, keyFile := testCertFiles(t)
	tokens := writeTokens(t, dir, "alice")
	main := fmt.Sprintf(`version: 1
tls:
  certFile: %s
  keyFile: %s
worlds:
%s`, certFile, keyFile, staticWorld("alice", testWorldID, tokens))
	configFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configFile, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := knowledgeconfig.Load(configFile)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	certs, err := certsource.Open(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An unreachable bucket. An empty one is no longer a failure: the
	// server creates that world and seeds its policy.
	newStore := func(_ context.Context, _ *knowledgeconfig.WorldConfig) (blob.Store, error) {
		return nil, errors.New("bucket unreachable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	group := &sync.WaitGroup{}
	// Cleanup, not defer: a failed expectation here must not deadlock the
	// package on background loops that outlive the test body.
	t.Cleanup(func() {
		cancel()
		group.Wait()
	})
	if _, err := newWorldManager(ctx, group, configFile, config, newStore, certs, slog.Default()); err == nil {
		t.Fatal("static mode must fail fast on an unopenable world")
	}
}
