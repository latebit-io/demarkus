package gateway

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/dispatch"
)

func TestWorldPoolClientForUnknownWorldReturnsErrWorldNotFound(t *testing.T) {
	cfg := &core.Config{Worlds: []core.WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := NewWorldPool(brokertest.Registry(t, cfg), fetch.Options{})
	_, _, err := pool.clientFor("team-b")
	var notFound *dispatch.WorldNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("clientFor unknown world: err = %v, want *dispatch.WorldNotFoundError", err)
	}
	if notFound.World != "team-b" {
		t.Errorf("WorldNotFoundError.World = %q, want team-b", notFound.World)
	}
}

func TestWorldPoolReusesClientForSameWorld(t *testing.T) {
	cfg := &core.Config{Worlds: []core.WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := NewWorldPool(brokertest.Registry(t, cfg), fetch.Options{})
	t.Cleanup(pool.Close)

	c1, host1, err := pool.clientFor("team-a")
	if err != nil {
		t.Fatalf("first clientFor: %v", err)
	}
	c2, host2, err := pool.clientFor("team-a")
	if err != nil {
		t.Fatalf("second clientFor: %v", err)
	}
	if c1 != c2 {
		t.Error("clientFor returned different *fetch.Client instances for the same world; want reused pointer")
	}
	if host1 != host2 {
		t.Errorf("host mismatch across clientFor calls: %q vs %q", host1, host2)
	}
}

func TestWorldPoolDispatchesUnknownWorldThroughTopLevelMethods(t *testing.T) {
	// Fetch, List and Versions return WorldNotFoundError as clientFor does:
	// the handlers render the tool error from that type.
	cfg := &core.Config{Worlds: []core.WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := NewWorldPool(brokertest.Registry(t, cfg), fetch.Options{})
	t.Cleanup(pool.Close)

	for _, tc := range []struct {
		name string
		fn   func() (fetch.Result, error)
	}{
		{"Fetch", func() (fetch.Result, error) {
			return pool.Fetch(t.Context(), fetch.FetchRequest{Host: "nope", Path: "/foo", Token: "tok"})
		}},
		{"List", func() (fetch.Result, error) {
			return pool.List(t.Context(), fetch.ListRequest{Host: "nope", Path: "/", Token: "tok"})
		}},
		{"Versions", func() (fetch.Result, error) {
			return pool.Versions(t.Context(), fetch.VersionsRequest{Host: "nope", Path: "/foo", Token: "tok"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn()
			if _, ok := errors.AsType[*dispatch.WorldNotFoundError](err); !ok {
				t.Errorf("%s with unknown world: err = %v, want *dispatch.WorldNotFoundError", tc.name, err)
			}
		})
	}
}

// A deprovisioned world stops being dialable at once, and the same name
// provisioned again gets a fresh client, not the old world's connections.
func TestWorldPoolLetsGoOfADroppedWorld(t *testing.T) {
	cfg := &core.Config{}
	pool := NewWorldPool(brokertest.Registry(t, cfg), fetch.Options{})
	defer pool.Close()
	eve := core.WorldConfig{Name: "eve-1", InternalAddress: "127.0.0.1:1", Generation: "t1"}
	brokertest.Registry(t, cfg).SetDynamic([]core.WorldConfig{eve}, nil)
	first, _, err := pool.clientFor("eve-1")
	if err != nil {
		t.Fatal(err)
	}

	brokertest.Registry(t, cfg).SetDynamic(nil, nil)
	var notFound *dispatch.WorldNotFoundError
	if _, _, err := pool.clientFor("eve-1"); !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want the dropped world unknown, not served from the cache", err)
	}

	eve.Generation = "t2"
	brokertest.Registry(t, cfg).SetDynamic([]core.WorldConfig{eve}, nil)
	if second, _, err := pool.clientFor("eve-1"); err != nil || second == first {
		t.Errorf("client after reprovisioning = %p (first %p), %v; want a new one", second, first, err)
	}
}

// reprovisionDuringFind makes the pool's next registry lookup race change: the
// lookup reads the registry, then change runs to completion (swap and drop
// listeners) before the pool sees the result.
func reprovisionDuringFind(pool *WorldPool, reg *core.WorldRegistry, change func()) {
	raced := false
	pool.find = func(name string) (core.WorldConfig, bool) {
		w, ok := reg.Find(name)
		if !raced {
			raced = true
			change()
		}
		return w, ok
	}
}

// A reprovision or deprovision whose drop completes between the pool's lookup
// and its cache insert leaves the current generation's route, or none, cached.
func TestWorldPoolLookupRacingADropCachesCurrentState(t *testing.T) {
	old := core.WorldConfig{Name: "eve-1", InternalAddress: "127.0.0.1:1", Generation: "t1"}
	next := core.WorldConfig{Name: "eve-1", InternalAddress: "127.0.0.1:2", Generation: "t2"}
	for _, tc := range []struct {
		name  string
		after []core.WorldConfig
		want  string // empty: the world is unknown
	}{
		{"reprovision", []core.WorldConfig{next}, next.Address()},
		{"deprovision", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &core.Config{}
			reg := brokertest.Registry(t, cfg)
			pool := NewWorldPool(reg, fetch.Options{})
			defer pool.Close()
			reg.SetDynamic([]core.WorldConfig{old}, nil)
			reprovisionDuringFind(pool, reg, func() { reg.SetDynamic(tc.after, nil) })
			for _, when := range []string{"racing", "after the race"} {
				_, host, err := pool.clientFor("eve-1")
				if tc.want == "" && !errors.As(err, new(*dispatch.WorldNotFoundError)) || tc.want != "" && (err != nil || host != tc.want) {
					t.Errorf("%s clientFor = %q, %v; want %q (empty: unknown world)", when, host, err, tc.want)
				}
				pool.find = reg.Find
			}
			pool.mu.Lock()
			defer pool.mu.Unlock()
			if n, want := len(pool.clients), len(tc.after); n != want {
				t.Errorf("pool holds %d clients, want %d", n, want)
			}
		})
	}
}

// Under concurrent lookups and reprovisions, once both settle every cached
// route is the current generation's.
func TestWorldPoolConcurrentReprovisionSettlesOnCurrentGeneration(t *testing.T) {
	cfg := &core.Config{}
	reg := brokertest.Registry(t, cfg)
	pool := NewWorldPool(reg, fetch.Options{})
	defer pool.Close()
	world := func(i int) core.WorldConfig {
		return core.WorldConfig{Name: "eve-1", InternalAddress: fmt.Sprintf("127.0.0.1:%d", 1000+i), Generation: fmt.Sprint(i)}
	}
	reg.SetDynamic([]core.WorldConfig{world(0)}, nil)

	const generations = 200
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := pool.clientFor("eve-1"); err != nil {
					t.Errorf("clientFor: %v", err)
					return
				}
			}
		})
	}
	for i := 1; i <= generations; i++ {
		reg.SetDynamic([]core.WorldConfig{world(i)}, nil)
	}
	close(stop)
	wg.Wait()

	want := world(generations)
	if _, host, err := pool.clientFor("eve-1"); err != nil || host != want.Address() {
		t.Errorf("settled clientFor = %q, %v; want %q", host, err, want.Address())
	}
}

// A closed pool and a shut down gateway leave the registry: neither is
// pinned by its drop listener, nor reopens a client for a later drop.
func TestPoolAndGatewayLeaveTheRegistryWhenClosed(t *testing.T) {
	cfg := brokertest.NewMemoryConfig()
	registry := brokertest.Registry(t, cfg)
	eve := core.WorldConfig{Name: "eve-1", Profile: core.ProfileMemory, InternalAddress: "127.0.0.1:1", Generation: "t1"}
	registry.SetDynamic([]core.WorldConfig{eve}, nil)

	pool := NewWorldPool(registry, fetch.Options{})
	if _, _, err := pool.clientFor("eve-1"); err != nil {
		t.Fatal(err)
	}
	g := newMemoryGateway(t, cfg, &fakeDispatcher{})
	g.memorySeed.stateFor("eve-1")
	pool.Close()
	if err := g.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	before := pool.drops
	registry.SetDynamic(nil, nil)
	if pool.drops != before {
		t.Error("a closed pool still heard a world drop")
	}
	if _, held := g.memorySeed.worlds["eve-1"]; !held {
		t.Error("a shut down gateway still heard a world drop")
	}
}
