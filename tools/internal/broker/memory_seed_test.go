package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
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

// policyDispatcher fakes a fresh world whose server already seeded a policy.
func policyDispatcher(policyMeta map[string]string) *fakeDispatcher {
	return &fakeDispatcher{
		FetchFn: func(_, path, _ string) (fetch.Result, error) {
			if path != publishpolicy.DocumentPath {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: policyMeta, Body: "# Policy\n"}}, nil
		},
	}
}

func TestEnsureMemorySeedReplacesServerSeededPolicy(t *testing.T) {
	d := policyDispatcher(map[string]string{"version": "1", "agent": publishpolicy.SeedAgent})
	g := newMemoryGateway(t, memoryTestConfig(), d)
	g.ensureMemorySeed(withAliceClaims(context.Background()), &g.srv.cfg.Worlds[0])

	calls := d.Calls().Publish
	if len(calls) != 3 || calls[0].Path != publishpolicy.DocumentPath {
		t.Fatalf("seed publishes = %+v, want the policy first of three", calls)
	}
	// Version two over the server's marked seed; never create-only, which
	// would conflict and leave the knowledge default in a memory world.
	if calls[0].ExpectedVersion != 1 {
		t.Errorf("policy expectedVersion = %d, want 1", calls[0].ExpectedVersion)
	}
}

func TestEnsureMemorySeedKeepsCuratedPolicy(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"written by a user":         {"version": "1", "agent": "alice@example.com"},
		"seed already replaced":     {"version": "2", "agent": "demarkus-memory-broker"},
		"marked seed edited later":  {"version": "3", "agent": publishpolicy.SeedAgent},
		"no agent on the first one": {"version": "1"},
	} {
		t.Run(name, func(t *testing.T) {
			d := policyDispatcher(meta)
			g := newMemoryGateway(t, memoryTestConfig(), d)
			g.ensureMemorySeed(withAliceClaims(context.Background()), &g.srv.cfg.Worlds[0])
			for _, call := range d.Calls().Publish {
				if call.Path == publishpolicy.DocumentPath {
					t.Errorf("policy republished at expected version %d", call.ExpectedVersion)
				}
			}
			if got := len(d.Calls().Publish); got != 2 {
				t.Errorf("published %d docs, want the template and the hub", got)
			}
		})
	}
}

// A server upgraded before its broker: the old broker's create-only policy
// publish conflicted with the server's seed, yet the hub still landed.
func TestEnsureMemorySeedReplacesServerSeedInSeededWorld(t *testing.T) {
	d := &fakeDispatcher{
		FetchFn: func(_, path, _ string) (fetch.Result, error) {
			meta := map[string]string{"version": "1"}
			if path == publishpolicy.DocumentPath {
				meta["agent"] = publishpolicy.SeedAgent
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: "# Doc\n"}}, nil
		},
	}
	g := newMemoryGateway(t, memoryTestConfig(), d)
	w := &g.srv.cfg.Worlds[0]
	g.ensureMemorySeed(withAliceClaims(context.Background()), w)

	calls := d.Calls().Publish
	if len(calls) != 1 || calls[0].Path != publishpolicy.DocumentPath || calls[0].ExpectedVersion != 1 {
		t.Fatalf("publishes into a seeded world = %+v, want only the policy at expected version 1", calls)
	}
	// Marked done: the next call adds no traffic.
	g.ensureMemorySeed(withAliceClaims(context.Background()), w)
	if got := len(d.Calls().Publish); got != 1 {
		t.Errorf("second call published again (%d total)", got)
	}
}
