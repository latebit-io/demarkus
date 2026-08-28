package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// freshWorldDispatcher fakes empty tenant worlds: everything is
// not-found until published (the fake's published map then serves it).
func freshWorldDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
}

func TestEnsureSoulSeedSeedsFreshWorld(t *testing.T) {
	d := freshWorldDispatcher()
	g := newMemoryGateway(t, memoryTestConfig(), d)
	w := &g.srv.cfg.Worlds[0] // alice-w

	g.ensureSoulSeed(withAliceClaims(context.Background()), w)

	d.mu.Lock()
	calls := append([]writeCall(nil), d.publishCalls...)
	d.mu.Unlock()
	wantOrder := []string{
		"/.well-known/demarkus/policy.md",
		"/.well-known/demarkus/template.md",
		"/index.md",
	}
	if len(calls) != len(wantOrder) {
		t.Fatalf("seed published %d docs, want %d: %+v", len(calls), len(wantOrder), calls)
	}
	for i, c := range calls {
		if c.worldName != "alice-w" {
			t.Errorf("seed publish %d went to world %q, want alice-w", i, c.worldName)
		}
		// /index.md is the seeded sentinel, so it must land last: a
		// crash mid-seed must leave the world retryable, not
		// half-seeded-but-marked-done.
		if c.path != wantOrder[i] {
			t.Errorf("seed publish %d path = %q, want %q", i, c.path, wantOrder[i])
		}
		if c.expectedVersion != 0 {
			t.Errorf("seed publish %d expectedVersion = %d, want 0 (create-only)", i, c.expectedVersion)
		}
		if c.meta["tags"] == "" {
			t.Errorf("seed publish %d has no tags", i)
		}
		if c.meta["agent"] != "demarkus-memory-broker" {
			t.Errorf("seed publish %d agent = %q, want demarkus-memory-broker", i, c.meta["agent"])
		}
		if strings.HasPrefix(c.body, "---") {
			t.Errorf("seed publish %d body opens with a frontmatter fence", i)
		}
		if !strings.HasPrefix(c.body, "# ") {
			t.Errorf("seed publish %d body does not open with an H1", i)
		}
	}

	// Second call: seeded fast path, no further traffic.
	g.ensureSoulSeed(withAliceClaims(context.Background()), w)
	d.mu.Lock()
	after := len(d.publishCalls)
	d.mu.Unlock()
	if after != len(wantOrder) {
		t.Errorf("second ensureSoulSeed published again (%d calls total)", after)
	}
}

func TestEnsureSoulSeedSkipsSeededWorld(t *testing.T) {
	d := seededDispatcher()
	g := newMemoryGateway(t, memoryTestConfig(), d)
	g.ensureSoulSeed(withAliceClaims(context.Background()), &g.srv.cfg.Worlds[0])
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.publishCalls) != 0 {
		t.Errorf("seeding published %d docs into an already-seeded world", len(d.publishCalls))
	}
}

// TestTenantGateSeedsOnFirstCall: the seeding rides the middleware, so
// a fresh tenant's very first tool call (even a read) seeds the soul.
func TestTenantGateSeedsOnFirstCall(t *testing.T) {
	d := freshWorldDispatcher()
	g := newMemoryGateway(t, memoryTestConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_fetch"])
	res, err := h(withAliceClaims(context.Background()), callToolReq("mark_fetch", map[string]any{"url": "mark://alice-w/index.md"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("first fetch on fresh world errored: %s", toolResultText(t, res))
	}
	if text := toolResultText(t, res); !strings.Contains(text, "# Soul") {
		t.Errorf("first fetch did not return the seeded hub:\n%s", text)
	}
	assertNoWorldTraffic(t, d, "bob-w")
}

// TestEnsureSoulSeedRetriesAfterFailure: a failed seed leaves the world
// unseeded, throttles immediate re-attempts, and retries once the
// throttle interval elapses.
func TestEnsureSoulSeedRetriesAfterFailure(t *testing.T) {
	fail := true
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			if fail {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusServerError}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": "1"}}}, nil
		},
	}
	g := newMemoryGateway(t, memoryTestConfig(), d)
	w := &g.srv.cfg.Worlds[0]

	g.ensureSoulSeed(withAliceClaims(context.Background()), w)
	g.soulSeed.mu.Lock()
	seeded := g.soulSeed.stateFor(w.Name).seeded
	g.soulSeed.mu.Unlock()
	if seeded {
		t.Fatal("world marked seeded after a failed publish")
	}

	// Within the throttle window a re-attempt is skipped entirely.
	fail = false
	d.mu.Lock()
	before := len(d.fetchCalls)
	d.mu.Unlock()
	g.ensureSoulSeed(withAliceClaims(context.Background()), w)
	d.mu.Lock()
	after := len(d.fetchCalls)
	d.mu.Unlock()
	if after != before {
		t.Fatalf("throttled window re-attempted seeding (%d new fetches)", after-before)
	}

	// Past the window the retry runs and seeds.
	g.soulSeed.mu.Lock()
	g.soulSeed.stateFor(w.Name).failedAt = time.Now().Add(-2 * soulSeedRetryInterval)
	g.soulSeed.mu.Unlock()
	g.ensureSoulSeed(withAliceClaims(context.Background()), w)
	g.soulSeed.mu.Lock()
	seeded = g.soulSeed.stateFor(w.Name).seeded
	g.soulSeed.mu.Unlock()
	if !seeded {
		t.Fatal("retry after failure did not seed the world")
	}
}
