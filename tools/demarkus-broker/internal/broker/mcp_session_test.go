package broker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pinnedClock returns a clock function whose time can be advanced
// via the returned setter. Used in idle-eviction and LRU tests so
// the time-based eviction logic is exercised deterministically
// without sleeping.
func pinnedClock(start time.Time) (now func() time.Time, advance func(time.Time)) {
	var mu sync.Mutex
	current := start
	now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance = func(t time.Time) {
		mu.Lock()
		defer mu.Unlock()
		current = t
	}
	return now, advance
}

func TestSessionCacheGetOrMintMissInvokesMint(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var calls int32
	mint := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&calls, 1)
		return cachedWorldToken{raw: "alice-team-a", expiresAt: clock().Add(time.Hour)}, nil
	}
	tok, fresh, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint)
	if err != nil {
		t.Fatalf("GetOrMint: %v", err)
	}
	if tok.raw != "alice-team-a" {
		t.Errorf("token.raw = %q, want alice-team-a", tok.raw)
	}
	if !fresh {
		t.Error("fresh = false on cache miss, want true")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("mint calls = %d, want 1", n)
	}
}

func TestSessionCacheGetOrMintHitReusesToken(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var calls int32
	mint := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&calls, 1)
		return cachedWorldToken{raw: "alice-team-a", expiresAt: clock().Add(time.Hour)}, nil
	}
	// First call mints.
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); err != nil {
		t.Fatalf("first GetOrMint: %v", err)
	}
	// Second call must hit the cache: same mint closure would
	// double-increment if invoked, so the counter pins the
	// no-mint property.
	tok, fresh, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint)
	if err != nil {
		t.Fatalf("second GetOrMint: %v", err)
	}
	if tok.raw != "alice-team-a" {
		t.Errorf("token.raw = %q, want alice-team-a (cache hit)", tok.raw)
	}
	if fresh {
		t.Error("fresh = true on cache hit, want false")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("mint calls = %d, want 1 (second call should hit cache)", n)
	}
}

func TestSessionCanonicalizesEmailKey(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var calls int32
	mint := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&calls, 1)
		return cachedWorldToken{raw: "alice-team-a", expiresAt: clock().Add(time.Hour)}, nil
	}
	// First call with mixed case + whitespace.
	if _, _, err := c.GetOrMint(context.Background(), "  Alice@Example.COM  ", "team-a", mint); err != nil {
		t.Fatalf("first GetOrMint: %v", err)
	}
	// Second call with canonical form should hit the cache, not
	// mint again. Same value the gateway's canonicalEmail produces.
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); err != nil {
		t.Fatalf("second GetOrMint: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("mint calls = %d, want 1 (canonicalization should collapse to one key)", n)
	}
}

func TestSessionCacheGetOrMintEmptyEmailReturnsError(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	mint := func(_ context.Context) (cachedWorldToken, error) {
		t.Fatal("mint must not be invoked for empty email")
		return cachedWorldToken{}, nil
	}
	if _, _, err := c.GetOrMint(context.Background(), "", "team-a", mint); !errors.Is(err, errInvalidEmail) {
		t.Errorf("GetOrMint with empty email: err = %v, want errInvalidEmail", err)
	}
	if _, _, err := c.GetOrMint(context.Background(), "   ", "team-a", mint); !errors.Is(err, errInvalidEmail) {
		t.Errorf("GetOrMint with whitespace email: err = %v, want errInvalidEmail", err)
	}
}

func TestSessionCacheInvalidateForcesReMint(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var calls int32
	mint := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&calls, 1)
		return cachedWorldToken{raw: "alice-team-a", expiresAt: clock().Add(time.Hour)}, nil
	}
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); err != nil {
		t.Fatalf("first GetOrMint: %v", err)
	}
	c.Invalidate("alice@example.com", "team-a")
	// Same identity, same world — invalidate drops the entry so
	// the next call must re-mint, and fresh=true again.
	_, fresh, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint)
	if err != nil {
		t.Fatalf("second GetOrMint: %v", err)
	}
	if !fresh {
		t.Error("fresh = false after invalidate, want true (cache should re-mint)")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("mint calls = %d, want 2", n)
	}
}

func TestSessionCacheExpiredTokenTreatedAsMiss(t *testing.T) {
	clock, advance := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var calls int32
	mint := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&calls, 1)
		return cachedWorldToken{raw: "alice-team-a", expiresAt: clock().Add(10 * time.Minute)}, nil
	}
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); err != nil {
		t.Fatalf("first GetOrMint: %v", err)
	}
	// Advance past the issued token's expiry.
	advance(clock().Add(20 * time.Minute))
	_, fresh, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint)
	if err != nil {
		t.Fatalf("second GetOrMint: %v", err)
	}
	if !fresh {
		t.Error("expired token treated as cache hit, want re-mint")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("mint calls = %d, want 2 (re-mint after expiry)", n)
	}
}

func TestSessionCacheSingleflightCoalescesConcurrentMints(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var calls int32
	// Buffered so the first (and only) sender doesn't deadlock
	// against a main goroutine that hasn't reached <-ready yet:
	// the closure can park the signal in the buffer and proceed
	// to wait on `release`. A select-default pattern would lose
	// the signal on a race; a buffered single-slot channel makes
	// the handshake order-independent.
	ready := make(chan struct{}, 1)
	release := make(chan struct{})
	mint := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&calls, 1)
		ready <- struct{}{}
		<-release
		return cachedWorldToken{raw: "alice-team-a", expiresAt: clock().Add(time.Hour)}, nil
	}

	const fanOut = 20
	var wg sync.WaitGroup
	tokens := make([]cachedWorldToken, fanOut)
	freshFlags := make([]bool, fanOut)
	errs := make([]error, fanOut)
	wg.Add(fanOut)
	for i := range fanOut {
		go func() {
			defer wg.Done()
			tokens[i], freshFlags[i], errs[i] = c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint)
		}()
	}
	// Wait for the first mint to be in flight before releasing.
	<-ready
	close(release)
	wg.Wait()

	for i := range fanOut {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: GetOrMint: %v", i, errs[i])
		}
		if tokens[i].raw != "alice-team-a" {
			t.Errorf("goroutine %d: token.raw = %q, want alice-team-a", i, tokens[i].raw)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("mint calls = %d, want 1 (singleflight should coalesce)", n)
	}
	// All callers must get the same token — that's the
	// coalescing property. A burst of N concurrent calls for one
	// (email, world) produces one mint and N callers see its
	// result.
	for i := 1; i < fanOut; i++ {
		if tokens[i].raw != tokens[0].raw {
			t.Errorf("goroutine %d token differs from goroutine 0 — singleflight did not deliver one shared result", i)
		}
	}
	// At least one caller must see fresh=true (whichever
	// goroutine drove the mint closure). Late-arriving callers
	// that landed after the cache was populated legitimately see
	// fresh=false. Asserting "exactly N fresh=true" would race
	// with scheduling — exact partitioning depends on whether a
	// caller's Do() landed during or after the closure ran.
	var freshCount int
	for _, f := range freshFlags {
		if f {
			freshCount++
		}
	}
	if freshCount < 1 {
		t.Errorf("fresh=true count = 0, want >= 1 (the mint-driving caller must see fresh=true)")
	}
}

func TestSessionCacheIdleEviction(t *testing.T) {
	clock, advance := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Minute, clock)
	mint := func(_ context.Context) (cachedWorldToken, error) {
		return cachedWorldToken{raw: "tok", expiresAt: clock().Add(time.Hour)}, nil
	}
	// Prime two sessions; advance well past the idle window;
	// trigger a third Get which evicts both idle entries before
	// inserting its own session.
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); err != nil {
		t.Fatalf("alice mint: %v", err)
	}
	if _, _, err := c.GetOrMint(context.Background(), "bob@example.com", "team-a", mint); err != nil {
		t.Fatalf("bob mint: %v", err)
	}
	if c.Len() != 2 {
		t.Fatalf("Len() = %d after two distinct mints, want 2", c.Len())
	}
	advance(clock().Add(2 * time.Minute))
	if _, _, err := c.GetOrMint(context.Background(), "carol@example.com", "team-a", mint); err != nil {
		t.Fatalf("carol mint: %v", err)
	}
	if c.Len() != 1 {
		t.Errorf("Len() = %d after idle eviction, want 1 (only carol survives)", c.Len())
	}
}

func TestSessionCacheLRUEvictionAtCap(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(2, time.Hour, clock)
	mint := func(_ context.Context) (cachedWorldToken, error) {
		return cachedWorldToken{raw: "tok", expiresAt: clock().Add(time.Hour)}, nil
	}
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); err != nil {
		t.Fatalf("alice mint: %v", err)
	}
	if _, _, err := c.GetOrMint(context.Background(), "bob@example.com", "team-a", mint); err != nil {
		t.Fatalf("bob mint: %v", err)
	}
	// Touch alice so bob becomes the LRU tail.
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); err != nil {
		t.Fatalf("alice second mint: %v", err)
	}
	// Insert carol — bob (LRU tail) gets evicted.
	if _, _, err := c.GetOrMint(context.Background(), "carol@example.com", "team-a", mint); err != nil {
		t.Fatalf("carol mint: %v", err)
	}
	// Membership check via the package-private sessions map.
	// Avoids using GetOrMint to inspect cache contents because
	// each GetOrMint mutates the LRU order and could evict the
	// very session under inspection. Direct map peek is the
	// cleanest no-side-effect probe.
	c.mu.Lock()
	_, aliceIn := c.sessions["alice@example.com"]
	_, bobIn := c.sessions["bob@example.com"]
	_, carolIn := c.sessions["carol@example.com"]
	gotLen := len(c.sessions)
	c.mu.Unlock()
	if gotLen != 2 {
		t.Errorf("len(sessions) = %d after LRU evict, want 2", gotLen)
	}
	if !aliceIn {
		t.Error("alice was evicted, want survived (most-recently-used)")
	}
	if bobIn {
		t.Error("bob survived LRU eviction, want evicted (least-recently-used)")
	}
	if !carolIn {
		t.Error("carol missing from cache after insert, want present")
	}
}

func TestSessionCacheMintErrorNotCached(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var calls int32
	mintErr := errors.New("simulated mint failure")
	mint := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&calls, 1)
		return cachedWorldToken{}, mintErr
	}
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); !errors.Is(err, mintErr) {
		t.Errorf("first GetOrMint: err = %v, want %v", err, mintErr)
	}
	// Second call should hit mint again — a failed mint must not
	// be cached, otherwise a transient kube blip would lock out
	// the user for the rest of the idle window.
	if _, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mint); !errors.Is(err, mintErr) {
		t.Errorf("second GetOrMint: err = %v, want %v", err, mintErr)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("mint calls = %d, want 2 (errors not cached)", n)
	}
}

func TestSessionCachePerWorldIsolation(t *testing.T) {
	clock, _ := pinnedClock(time.Unix(0, 0).UTC())
	c := newSessionCache(10, time.Hour, clock)
	var teamACalls, teamBCalls int32
	mintTeamA := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&teamACalls, 1)
		return cachedWorldToken{raw: "alice-team-a", expiresAt: clock().Add(time.Hour)}, nil
	}
	mintTeamB := func(_ context.Context) (cachedWorldToken, error) {
		atomic.AddInt32(&teamBCalls, 1)
		return cachedWorldToken{raw: "alice-team-b", expiresAt: clock().Add(time.Hour)}, nil
	}
	// Same email, different worlds — each world keeps its own
	// cached token. Invalidating one world must not touch the
	// other.
	tA, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-a", mintTeamA)
	if err != nil {
		t.Fatalf("team-a mint: %v", err)
	}
	tB, _, err := c.GetOrMint(context.Background(), "alice@example.com", "team-b", mintTeamB)
	if err != nil {
		t.Fatalf("team-b mint: %v", err)
	}
	if tA.raw == tB.raw {
		t.Errorf("per-world tokens collapsed: team-a=%q, team-b=%q", tA.raw, tB.raw)
	}
	c.Invalidate("alice@example.com", "team-a")
	// team-b cache must still hit.
	_, fresh, err := c.GetOrMint(context.Background(), "alice@example.com", "team-b", mintTeamB)
	if err != nil {
		t.Fatalf("team-b cache check: %v", err)
	}
	if fresh {
		t.Error("team-b was re-minted after team-a invalidate, want cache hit")
	}
	if n := atomic.LoadInt32(&teamBCalls); n != 1 {
		t.Errorf("team-b mint calls = %d, want 1", n)
	}
}
