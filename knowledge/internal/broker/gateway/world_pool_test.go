package gateway

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

func TestWorldPoolClientForUnknownWorldReturnsErrWorldNotFound(t *testing.T) {
	cfg := &core.Config{Worlds: []core.WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := NewWorldPool(cfg.Registry(), fetch.Options{})
	_, _, err := pool.clientFor("team-b")
	var notFound *errWorldNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("clientFor unknown world: err = %v, want *errWorldNotFound", err)
	}
	if notFound.worldName != "team-b" {
		t.Errorf("errWorldNotFound.worldName = %q, want team-b", notFound.worldName)
	}
}

func TestWorldPoolReusesClientForSameWorld(t *testing.T) {
	cfg := &core.Config{Worlds: []core.WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := NewWorldPool(cfg.Registry(), fetch.Options{})
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
	// The dispatcher-shaped surface (Fetch/List/Versions) must
	// surface errWorldNotFound the same way clientFor does — the
	// handlers depend on the error type to render a useful tool
	// error envelope.
	cfg := &core.Config{Worlds: []core.WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := NewWorldPool(cfg.Registry(), fetch.Options{})
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
			if _, ok := errors.AsType[*errWorldNotFound](err); !ok {
				t.Errorf("%s with unknown world: err = %v, want *errWorldNotFound", tc.name, err)
			}
		})
	}
}

// A deprovisioned world stops being dialable at once, and the same name
// provisioned again gets a fresh client, not the old world's connections.
func TestWorldPoolLetsGoOfADroppedWorld(t *testing.T) {
	cfg := &core.Config{}
	pool := NewWorldPool(cfg.Registry(), fetch.Options{})
	defer pool.Close()
	eve := core.WorldConfig{Name: "eve-1", InternalAddress: "127.0.0.1:1", Generation: "t1"}
	cfg.Registry().SetDynamic([]core.WorldConfig{eve}, nil)
	first, _, err := pool.clientFor("eve-1")
	if err != nil {
		t.Fatal(err)
	}

	cfg.Registry().SetDynamic(nil, nil)
	var notFound *errWorldNotFound
	if _, _, err := pool.clientFor("eve-1"); !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want the dropped world unknown, not served from the cache", err)
	}

	eve.Generation = "t2"
	cfg.Registry().SetDynamic([]core.WorldConfig{eve}, nil)
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
			reg := cfg.Registry()
			pool := NewWorldPool(reg, fetch.Options{})
			defer pool.Close()
			reg.SetDynamic([]core.WorldConfig{old}, nil)
			reprovisionDuringFind(pool, reg, func() { reg.SetDynamic(tc.after, nil) })
			for _, when := range []string{"racing", "after the race"} {
				_, host, err := pool.clientFor("eve-1")
				if tc.want == "" && !errors.As(err, new(*errWorldNotFound)) || tc.want != "" && (err != nil || host != tc.want) {
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
	reg := cfg.Registry()
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
