package broker

import (
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
)

func TestResolveWorldAddressDefaultDNSPattern(t *testing.T) {
	w := &WorldConfig{Name: "team-a", Namespace: "team-a"}
	got := resolveWorldAddress(w)
	want := "team-a.team-a.svc.cluster.local:6309"
	if got != want {
		t.Errorf("resolveWorldAddress = %q, want %q", got, want)
	}
}

func TestResolveWorldAddressNamespaceDiffersFromName(t *testing.T) {
	// When operators deploy multiple worlds in a shared
	// namespace, the chart's default Service-DNS form still
	// applies because the Service name equals the world's Name.
	w := &WorldConfig{Name: "team-b", Namespace: "shared-worlds"}
	got := resolveWorldAddress(w)
	want := "team-b.shared-worlds.svc.cluster.local:6309"
	if got != want {
		t.Errorf("resolveWorldAddress = %q, want %q", got, want)
	}
}

func TestResolveWorldAddressInternalAddressOverride(t *testing.T) {
	// The plan v3 escape hatch: when an operator's Service name
	// diverges from the world's Name (rare; the chart keeps them
	// aligned by default), InternalAddress wins outright.
	w := &WorldConfig{
		Name:            "team-c",
		Namespace:       "team-c",
		InternalAddress: "team-c-mark.platform:7000",
	}
	got := resolveWorldAddress(w)
	want := "team-c-mark.platform:7000"
	if got != want {
		t.Errorf("resolveWorldAddress with override = %q, want %q", got, want)
	}
}

func TestWorldPoolClientForUnknownWorldReturnsErrWorldNotFound(t *testing.T) {
	cfg := &Config{Worlds: []WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := newWorldPool(cfg, fetch.Options{})
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
	cfg := &Config{Worlds: []WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := newWorldPool(cfg, fetch.Options{})
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
	cfg := &Config{Worlds: []WorldConfig{{Name: "team-a", Namespace: "team-a"}}}
	pool := newWorldPool(cfg, fetch.Options{})
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
			var notFound *errWorldNotFound
			if !errors.As(err, &notFound) {
				t.Errorf("%s with unknown world: err = %v, want *errWorldNotFound", tc.name, err)
			}
		})
	}
}

// A deprovisioned world stops being dialable at once, and the same name
// provisioned again gets a fresh client, not the old world's connections.
func TestWorldPoolLetsGoOfADroppedWorld(t *testing.T) {
	cfg := &Config{}
	pool := newWorldPool(cfg, fetch.Options{})
	defer pool.Close()
	eve := WorldConfig{Name: "eve-1", InternalAddress: "127.0.0.1:1", generation: "t1"}
	cfg.worlds().SetDynamic([]WorldConfig{eve}, nil)
	first, _, err := pool.clientFor("eve-1")
	if err != nil {
		t.Fatal(err)
	}

	cfg.worlds().SetDynamic(nil, nil)
	var notFound *errWorldNotFound
	if _, _, err := pool.clientFor("eve-1"); !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want the dropped world unknown, not served from the cache", err)
	}

	eve.generation = "t2"
	cfg.worlds().SetDynamic([]WorldConfig{eve}, nil)
	if second, _, err := pool.clientFor("eve-1"); err != nil || second == first {
		t.Errorf("client after reprovisioning = %p (first %p), %v; want a new one", second, first, err)
	}
}
