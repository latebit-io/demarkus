package knowledgeserver

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/peerhint"
	"github.com/latebit-io/demarkus/server/internal/worldstore"
	"github.com/latebit-io/demarkus/server/knowledgeserver/bucketworlds"
)

// gatedStores is the harness's bucket factory with a gate per world: an open
// of a gated world fails with its error, or parks until its channel closes
// or the open's context ends.
type gatedStores struct {
	h  *worldsTestHarness
	mu sync.Mutex
	// fail and park are keyed by world name.
	fail    map[string]error
	park    map[string]chan struct{}
	entered chan string
	opens   []string
}

func newGatedStores(h *worldsTestHarness) *gatedStores {
	return &gatedStores{h: h, fail: map[string]error{}, park: map[string]chan struct{}{}, entered: make(chan string, 16)}
}

func (g *gatedStores) set(world string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		delete(g.fail, world)
		return
	}
	g.fail[world] = err
}

func (g *gatedStores) parkOpens(world string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	release := make(chan struct{})
	g.park[world] = release
	return release
}

func (g *gatedStores) opened() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.opens)
}

func (g *gatedStores) factory() worldstore.Factory {
	return bucketworlds.ForWorlds(func(ctx context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error) {
		g.mu.Lock()
		g.opens = append(g.opens, world.Name)
		err, release := g.fail[world.Name], g.park[world.Name]
		g.mu.Unlock()
		if release != nil {
			g.entered <- world.Name
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err != nil {
			return nil, err
		}
		return g.h.memory(world.Name)
	})
}

// openGated opens a harness over fragment (resilient) or an inline world
// list (static) with gated stores.
func openGated(t *testing.T, worlds string, resilient bool) (*worldsTestHarness, *gatedStores) {
	t.Helper()
	h := newHarness(t.TempDir())
	stores := newGatedStores(h)
	section := "worlds:\n" + worlds
	if resilient {
		h.writeFragment(t, section)
		section = "worldsFile: worlds.yaml\n"
	}
	if err := h.open(t, section, stores.factory()); err != nil {
		t.Fatalf("open harness: %v", err)
	}
	return h, stores
}

var errStoreDown = errors.New("store unreachable")

// A replacement that fails to open must not take down the world it would
// have replaced: the live runtime keeps serving its config and its route.
func TestFailedReplacementKeepsTheLiveWorldServing(t *testing.T) {
	for _, resilient := range []bool{true, false} {
		tokens := writeTokens(t, t.TempDir(), "alice")
		h, stores := openGated(t, worldFragment("alice", testWorldID, tokens, true), resilient)
		live := h.runtime(t, "alice")
		var retired []string
		h.manager.beforeRetire = func(name string) { retired = append(retired, name) }

		stores.set("alice", errStoreDown)
		err := h.manager.apply(changedWorlds(h, "alice"))
		if resilient && err != nil {
			t.Fatalf("resilient apply: %v", err)
		}
		if !resilient && !errors.Is(err, errStoreDown) {
			t.Fatalf("static apply: err = %v, want the open failure", err)
		}
		if len(retired) != 0 || h.runtime(t, "alice") != live {
			t.Fatalf("resilient=%v: the live world was retired (%v) or swapped on a failed replacement", resilient, retired)
		}
		if !h.routes("alice.memory.svc.cluster.local") {
			t.Fatalf("resilient=%v: the world lost its route on a failed replacement", resilient)
		}

		// The store recovers; the replacement lands and only then the old one goes.
		stores.set("alice", nil)
		if resilient {
			h.manager.retryPending()
		} else if err := h.manager.apply(changedWorlds(h, "alice")); err != nil {
			t.Fatalf("apply after recovery: %v", err)
		}
		if h.runtime(t, "alice") == live || !slices.Equal(retired, []string{"alice"}) {
			t.Fatalf("resilient=%v: replacement did not take over after recovery (retired %v)", resilient, retired)
		}
		if got := h.world(t, "alice").Limits.MaxWatches; got != 512 {
			t.Fatalf("live config MaxWatches = %d, want the replacement's 512", got)
		}
	}
}

// changedWorlds is the harness's world set with name's watch cap lowered.
func changedWorlds(h *worldsTestHarness, name string) []knowledgeconfig.WorldConfig {
	worlds := slices.Clone(h.config.Worlds)
	for index := range worlds {
		if worlds[index].Name == name {
			worlds[index].Limits.MaxWatches = 512
		}
	}
	return worlds
}

// Worlds open outside the manager's lock: a store that hangs must not hold
// up peer hints, a drain or a count, and shutdown must end the open.
func TestSlowOpenDoesNotHoldTheManagerLockAndShutdownEndsIt(t *testing.T) {
	dir := t.TempDir()
	h, stores := openGated(t, worldFragment("alice", testWorldID, writeTokens(t, dir, "alice"), true), true)
	stores.parkOpens("bob")
	both := append(slices.Clone(h.config.Worlds), bobWorld(t, h, dir))

	applied := make(chan error, 1)
	go func() { applied <- h.manager.apply(both) }()
	<-stores.entered

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.manager.Hint(peerhint.Hint{WorldID: testWorldID, Sequence: 1})
		h.manager.Drain()
		if got := h.manager.WorldCount(); got != 1 {
			t.Errorf("WorldCount during a slow open = %d, want 1", got)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hint, drain or count waited on a world that is still opening")
	}

	// Shutdown: the watch context ends, and with it the open.
	h.stop()
	select {
	case err := <-applied:
		if err != nil {
			t.Fatalf("apply after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not end the pending open")
	}
}

// bobWorld is a second bucket world for the harness.
func bobWorld(t *testing.T, h *worldsTestHarness, tokensDir string) knowledgeconfig.WorldConfig {
	t.Helper()
	bob := *h.world(t, "alice")
	bob.Name, bob.Authorities = "bob", []string{"bob.memory.svc.cluster.local"}
	bob.Bucket.URL, bob.Bucket.WorldID = "gs://memory-bob", testWorldIDB
	bob.Auth.TokensFile = writeTokens(t, tokensDir, "bob")
	return bob
}

// A world already pending is the retry loop's: an apply that does not
// change it must not wait on its unreachable store again.
func TestApplyLeavesAnUnchangedPendingWorldToTheRetryLoop(t *testing.T) {
	dir := t.TempDir()
	h, stores := openGated(t, worldFragment("alice", testWorldID, writeTokens(t, dir, "alice"), true), true)
	stores.set("bob", errStoreDown)
	both := append(slices.Clone(h.config.Worlds), bobWorld(t, h, dir))
	if err := h.manager.apply(both); err != nil {
		t.Fatal(err)
	}
	before := len(stores.opened())
	if err := h.manager.apply(both); err != nil {
		t.Fatal(err)
	}
	if got := len(stores.opened()); got != before {
		t.Fatalf("an unchanged apply opened %d stores again; the pending world is the retry loop's", got-before)
	}
	stores.set("bob", nil)
	h.manager.retryPending()
	if !h.routes("bob.memory.svc.cluster.local") {
		t.Fatal("the retry loop did not bring the pending world up")
	}
}

// A world set applied after Close must not reopen worlds nobody will close.
func TestApplyAfterCloseIsRefused(t *testing.T) {
	h, stores := openGated(t, worldFragment("alice", testWorldID, writeTokens(t, t.TempDir(), "alice"), true), true)
	worlds := slices.Clone(h.config.Worlds)
	h.manager.Close()
	before := len(stores.opened())
	if err := h.manager.apply(worlds); !errors.Is(err, errManagerClosed) {
		t.Fatalf("apply after Close: err = %v, want errManagerClosed", err)
	}
	h.manager.retryPending()
	if got := len(stores.opened()); got != before || h.manager.WorldCount() != 0 {
		t.Fatalf("a closed manager opened %d stores and holds %d worlds", got-before, h.manager.WorldCount())
	}
}

// The broker's private worlds must be gated on every world set, not only at
// start: an ungated one is refused, and what is live under its name stays.
func TestRequireGateRefusesUngatedWorldsOnEveryApply(t *testing.T) {
	dir := t.TempDir()
	gated := worldFragment("alice", testWorldID, writeTokens(t, dir, "alice"), true) + "    gated: true\n"
	h, _ := openGated(t, gated, true)
	private := func(authority string) bool { return strings.HasSuffix(authority, ".memory.svc.cluster.local") }
	if err := h.manager.requireGate(private); err != nil {
		t.Fatalf("requireGate over gated worlds: %v", err)
	}
	live := h.runtime(t, "alice")

	// A reload ungates alice and adds an ungated bob.
	ungated := slices.Clone(h.config.Worlds)
	ungated[0].Gated = false
	ungated = append(ungated, bobWorld(t, h, dir))
	ungated[1].Gated = false
	if err := h.manager.apply(ungated); err != nil {
		t.Fatalf("resilient apply: %v", err)
	}
	if h.runtime(t, "alice") != live || !h.world(t, "alice").Gated {
		t.Fatal("an ungating reload replaced the gated runtime")
	}
	if h.routes("bob.memory.svc.cluster.local") {
		t.Fatal("an ungated private world was opened and routed")
	}

	// Gated, bob is admitted.
	ungated[1].Gated = true
	if err := h.manager.apply([]knowledgeconfig.WorldConfig{*h.world(t, "alice"), ungated[1]}); err != nil {
		t.Fatal(err)
	}
	if !h.routes("bob.memory.svc.cluster.local") {
		t.Fatal("the gated world was not opened")
	}
}

// Installing the rule over a world already served ungated is an error: the
// broker must not start beside a private world on the capability listener.
func TestRequireGateFailsOverALiveUngatedWorld(t *testing.T) {
	h, _ := openGated(t, worldFragment("alice", testWorldID, writeTokens(t, t.TempDir(), "alice"), true), false)
	err := h.manager.requireGate(func(string) bool { return true })
	if !errors.Is(err, errUngated) || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("requireGate over an ungated live world: err = %v", err)
	}
	// Static mode refuses the world set outright.
	if err := h.manager.apply(changedWorlds(h, "alice")); !errors.Is(err, errUngated) {
		t.Fatalf("static apply of an ungated private world: err = %v", err)
	}
}

// A file store is its directory's one writer: the old one closes before the
// replacement opens, and a replacement that cannot open brings it back.
func TestFileWorldReplacementNeverOpensTwoStores(t *testing.T) {
	dir := t.TempDir()
	tokens := writeTokens(t, dir, "team")
	h := newHarness(dir)
	files := worldStores(nil)
	var mu sync.Mutex
	open, most := 0, 0
	var failNext error
	counting := func(ctx context.Context, world *knowledgeconfig.WorldConfig, hooks worldstore.Hooks) (worldstore.Store, error) {
		mu.Lock()
		err := failNext
		failNext = nil
		mu.Unlock()
		if err != nil {
			return nil, err
		}
		store, err := files(ctx, world, hooks)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		open++
		most = max(most, open)
		mu.Unlock()
		return &countedStore{Store: store, closed: func() {
			mu.Lock()
			open--
			mu.Unlock()
		}}, nil
	}
	if err := h.open(t, "worlds:\n"+fileWorldFragment("team", filepath.Join(dir, "team"), tokens, false), counting); err != nil {
		t.Fatal(err)
	}

	changed := slices.Clone(h.config.Worlds)
	changed[0].Limits.MaxWatches = 512
	if err := h.manager.apply(changed); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if most != 1 {
		t.Fatalf("%d file stores were open on one directory at once", most)
	}
	if !h.routes("team.example.com") || h.world(t, "team").Limits.MaxWatches != 512 {
		t.Fatal("the replacement is not serving")
	}

	// The next replacement cannot open: the previous config comes back.
	mu.Lock()
	failNext = errStoreDown
	mu.Unlock()
	changed[0].Limits.MaxWatches = 256
	if err := h.manager.apply(changed); !errors.Is(err, errStoreDown) {
		t.Fatalf("failed replace: err = %v, want the open failure", err)
	}
	if most != 1 {
		t.Fatalf("%d file stores were open on one directory at once", most)
	}
	if !h.routes("team.example.com") || h.world(t, "team").Limits.MaxWatches != 512 {
		t.Fatalf("after a failed replacement the world serves MaxWatches %d routed=%v; want the previous 512, routed",
			h.world(t, "team").Limits.MaxWatches, h.routes("team.example.com"))
	}
}

// countedStore reports its close.
type countedStore struct {
	worldstore.Store
	closed func()
}

func (c *countedStore) Close() error {
	err := c.Store.Close()
	c.closed()
	return err
}
