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
		FetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
}

func TestEnsureMemorySeedSeedsFreshWorld(t *testing.T) {
	d := freshWorldDispatcher()
	g := newMemoryGateway(t, memoryTestConfig(), d)
	w := &g.srv.cfg.Worlds[0] // alice-w

	g.ensureMemorySeed(withAliceClaims(context.Background()), w)

	calls := d.Calls().Publish
	wantOrder := []string{
		"/.well-known/demarkus/policy.md",
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
		if c.Meta["tags"] == "" {
			t.Errorf("seed publish %d has no tags", i)
		}
		if c.Meta["agent"] != "demarkus-memory-broker" {
			t.Errorf("seed publish %d agent = %q, want demarkus-memory-broker", i, c.Meta["agent"])
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
	g := newMemoryGateway(t, memoryTestConfig(), d)
	g.ensureMemorySeed(withAliceClaims(context.Background()), &g.srv.cfg.Worlds[0])
	if published := d.Calls().Publish; len(published) != 0 {
		t.Errorf("seeding published %d docs into an already-seeded world", len(published))
	}
}

// TestTenantGateSeedsOnFirstCall: the seeding rides the middleware, so
// a fresh tenant's very first tool call (even a read) seeds the memory.
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
		FetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		PublishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			if fail {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusServerError}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": "1"}}}, nil
		},
	}
	g := newMemoryGateway(t, memoryTestConfig(), d)
	w := &g.srv.cfg.Worlds[0]

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
