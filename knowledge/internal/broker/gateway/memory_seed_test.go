package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// freshWorldDispatcher fakes empty tenant worlds: everything is
// not-found until published (the fake's published map then serves it).
func freshWorldDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
}

func TestEnsureMemorySeedSeedsFreshWorld(t *testing.T) {
	d := freshWorldDispatcher()
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	w := &g.deps.Worlds.All()[0] // alice-w

	g.ensureMemorySeed(withAliceClaims(context.Background()), w)

	calls := d.Calls().Publish
	// No policy: the server seeds it when it opens the world.
	wantOrder := []string{
		"/.well-known/demarkus/template.md",
		"/index.md",
	}
	if len(calls) != len(wantOrder) {
		t.Fatalf("seed published %d docs, want %d: %+v", len(calls), len(wantOrder), calls)
	}
	for i, c := range calls {
		if c.Host != "alice-w" {
			t.Errorf("seed publish %d went to world %q, want alice-w", i, c.Host)
		}
		// /index.md is the seeded sentinel, so it must land last: a
		// crash mid-seed must leave the world retryable, not
		// half-seeded-but-marked-done.
		if c.Path != wantOrder[i] {
			t.Errorf("seed publish %d path = %q, want %q", i, c.Path, wantOrder[i])
		}
		if c.ExpectedVersion != 0 {
			t.Errorf("seed publish %d expectedVersion = %d, want 0 (create-only)", i, c.ExpectedVersion)
		}
		if c.Metadata["tags"] == "" {
			t.Errorf("seed publish %d has no tags", i)
		}
		if c.Metadata["agent"] != "demarkus-memory-broker" {
			t.Errorf("seed publish %d agent = %q, want demarkus-memory-broker", i, c.Metadata["agent"])
		}
		if strings.HasPrefix(c.Body, "---") {
			t.Errorf("seed publish %d body opens with a frontmatter fence", i)
		}
		if !strings.HasPrefix(c.Body, "# ") {
			t.Errorf("seed publish %d body does not open with an H1", i)
		}
	}

	// Second call: seeded fast path, no further traffic.
	g.ensureMemorySeed(withAliceClaims(context.Background()), w)
	after := len(d.Calls().Publish)
	if after != len(wantOrder) {
		t.Errorf("second ensureMemorySeed published again (%d calls total)", after)
	}
}

func TestEnsureMemorySeedSkipsSeededWorld(t *testing.T) {
	d := seededDispatcher()
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	g.ensureMemorySeed(withAliceClaims(context.Background()), &g.deps.Worlds.All()[0])
	if published := d.Calls().Publish; len(published) != 0 {
		t.Errorf("seeding published %d docs into an already-seeded world", len(published))
	}
}

// TestTenantGateSeedsOnFirstCall: the seeding rides the middleware, so
// a fresh tenant's very first tool call (even a read) seeds the memory.
func TestTenantGateSeedsOnFirstCall(t *testing.T) {
	d := freshWorldDispatcher()
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_fetch"])
	res, err := h(withAliceClaims(context.Background()), callToolReq("mark_fetch", map[string]any{"url": "mark://alice-w/index.md"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("first fetch on fresh world errored: %s", toolResultText(t, res))
	}
	if text := toolResultText(t, res); !strings.Contains(text, "# Memory") {
		t.Errorf("first fetch did not return the seeded hub:\n%s", text)
	}
	assertNoWorldTraffic(t, d, "bob-w")
}

// TestEnsureMemorySeedRetriesAfterFailure: a failed seed leaves the world
// unseeded, throttles immediate re-attempts, and retries once the
// throttle interval elapses.
func TestEnsureMemorySeedRetriesAfterFailure(t *testing.T) {
	fail := true
	d := &fakeDispatcher{
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			if fail {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusServerError}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": "1"}}}, nil
		},
	}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	w := &g.deps.Worlds.All()[0]

	g.ensureMemorySeed(withAliceClaims(context.Background()), w)
	g.memorySeed.mu.Lock()
	seeded := g.memorySeed.stateFor(w.Name).seeded
	g.memorySeed.mu.Unlock()
	if seeded {
		t.Fatal("world marked seeded after a failed publish")
	}

	// Within the throttle window a re-attempt is skipped entirely.
	fail = false
	before := d.FetchCallCount()
	g.ensureMemorySeed(withAliceClaims(context.Background()), w)
	after := d.FetchCallCount()
	if after != before {
		t.Fatalf("throttled window re-attempted seeding (%d new fetches)", after-before)
	}

	// Past the window the retry runs and seeds.
	g.memorySeed.mu.Lock()
	g.memorySeed.stateFor(w.Name).failedAt = time.Now().Add(-2 * memorySeedRetryInterval)
	g.memorySeed.mu.Unlock()
	g.ensureMemorySeed(withAliceClaims(context.Background()), w)
	g.memorySeed.mu.Lock()
	seeded = g.memorySeed.stateFor(w.Name).seeded
	g.memorySeed.mu.Unlock()
	if !seeded {
		t.Fatal("retry after failure did not seed the world")
	}
}

// policyDispatcher fakes a fresh world whose server already seeded a policy.
func policyDispatcher(policyMeta map[string]string) *fakeDispatcher {
	return &fakeDispatcher{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			path := r.Path
			if path != publishpolicy.DocumentPath {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: policyMeta, Body: "# Policy\n"}}, nil
		},
	}
}

// The policy is the server's to seed and to upgrade: whatever policy a world
// holds, the server's own default at version one included, the gateway
// never writes one.
func TestEnsureMemorySeedNeverWritesThePolicy(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"the server's default seed": {"version": "1", "agent": publishpolicy.SeedAgent},
		"the server's memory seed":  {"version": "1", "agent": "demarkus-memory-broker"},
		"written by a user":         {"version": "1", "agent": "alice@example.com"},
		"edited later":              {"version": "3", "agent": publishpolicy.SeedAgent},
	} {
		t.Run(name, func(t *testing.T) {
			d := policyDispatcher(meta)
			g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
			g.ensureMemorySeed(withAliceClaims(context.Background()), &g.deps.Worlds.All()[0])
			for _, call := range d.Calls().Publish {
				if call.Path == publishpolicy.DocumentPath {
					t.Errorf("policy published at expected version %d", call.ExpectedVersion)
				}
				if call.ExpectedVersion != 0 {
					t.Errorf("%s published at expected version %d, want create-only", call.Path, call.ExpectedVersion)
				}
			}
			if got := len(d.Calls().Publish); got != 2 {
				t.Errorf("published %d docs, want the template and the hub", got)
			}
		})
	}
}

// A world holding the hub is seeded: nothing is written into it, whatever
// policy it holds.
func TestEnsureMemorySeedLeavesASeededWorldAlone(t *testing.T) {
	d := &fakeDispatcher{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			meta := map[string]string{"version": "1"}
			if r.Path == publishpolicy.DocumentPath {
				meta["agent"] = publishpolicy.SeedAgent
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: "# Doc\n"}}, nil
		},
	}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	g.ensureMemorySeed(withAliceClaims(context.Background()), &g.deps.Worlds.All()[0])
	if calls := d.Calls().Publish; len(calls) != 0 {
		t.Fatalf("publishes into a seeded world = %+v, want none", calls)
	}
}

// The same identity provisioned again gets the same world name and an empty
// bucket. A pod that remembered "seeded" for that name would never seed it.
func TestReprovisionedWorldIsSeededAgain(t *testing.T) {
	cfg := brokertest.NewMemoryConfig()
	eve := core.WorldConfig{Name: "eve-1", Profile: core.ProfileMemory, Namespace: "memory", Allow: core.AllowConfig{Emails: []string{"alice@example.com"}}, Generation: "t1"}
	brokertest.Registry(t, cfg).SetDynamic([]core.WorldConfig{eve}, nil)
	d := freshWorldDispatcher()
	g := newMemoryGateway(t, cfg, d)
	ctx := withAliceClaims(context.Background())

	g.ensureMemorySeed(ctx, &eve)
	seededOnce := len(d.Calls().Publish)
	g.ensureMemorySeed(ctx, &eve)
	if seededOnce == 0 || len(d.Calls().Publish) != seededOnce {
		t.Fatalf("publishes = %d then %d, want one seeding", seededOnce, len(d.Calls().Publish))
	}

	brokertest.Registry(t, cfg).SetDynamic(nil, nil) // deprovisioned
	d.Published = nil                                // its bucket went with it
	eve.Generation = "t2"
	brokertest.Registry(t, cfg).SetDynamic([]core.WorldConfig{eve}, nil)
	g.ensureMemorySeed(ctx, &eve)
	if got := len(d.Calls().Publish); got != 2*seededOnce {
		t.Errorf("publishes = %d, want the new world seeded too (%d)", got, 2*seededOnce)
	}
}

// The seed writes under the broker's own grant: govern for the template,
// scoped to exactly the seeded paths, never the tenant's grant.
func TestEnsureMemorySeedWritesUnderTheBrokerGovernGrant(t *testing.T) {
	d := policyDispatcher(map[string]string{"version": "1", "agent": publishpolicy.SeedAgent})
	var grants []protocol.Grant
	d.PublishFn = func(ctx context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
		grant, _ := protocol.GrantFrom(ctx)
		grants = append(grants, grant)
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated}}, nil
	}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	g.ensureMemorySeed(withAliceClaims(context.Background()), &g.deps.Worlds.All()[0])
	if len(grants) != 2 {
		t.Fatalf("seed publishes = %d, want 2", len(grants))
	}
	for _, grant := range grants {
		if !grant.Govern || grant.Label != "demarkus-memory-broker" || strings.Join(grant.Paths, ",") != "/.well-known/demarkus/template.md,/index.md" {
			t.Errorf("seed grant = %+v", grant)
		}
	}
}
