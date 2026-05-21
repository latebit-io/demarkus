package broker

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// cachedWorldToken is one entry in a session's per-world token map.
// raw is the world access-token in plaintext (never persisted to
// disk); expiresAt mirrors Issuer.MintResult.ExpiresAt so the cache
// can drop entries past their natural lifetime without re-asking
// the world. ExpiresAt is the broker's own clock — the world's
// view of expiry comes from the token payload it validates per
// request, so a small clock skew between broker and world is
// absorbed by the world's RFC3339 deserialization rather than the
// cache.
type cachedWorldToken struct {
	raw       string
	expiresAt time.Time
}

// expired returns true when the token has aged past expiresAt
// relative to the supplied clock. A zero expiresAt is treated as
// "never expires" so a future ExpiresAfter-less mint path doesn't
// accidentally evict everything on the first access.
func (c cachedWorldToken) expired(now time.Time) bool {
	return !c.expiresAt.IsZero() && !now.Before(c.expiresAt)
}

// session is the per-canonical-email cache slice. worlds maps
// world name → cached token; lastUsed advances on every Get so
// idle eviction can find the stalest entries from the LRU tail.
// lruElem is the list.Element back-pointer used by sessionCache
// to evict in O(1).
type session struct {
	email    string
	worlds   map[string]cachedWorldToken
	lastUsed time.Time
	lruElem  *list.Element
}

// sessionCache holds the per-email map of world access-tokens the
// MCP gateway mints lazily on first tool call. Lifecycle:
//
//   - Cache key is canonicalEmail(claims.Email) — same canonical
//     form Issuer.MintFiltered uses, so the broker has exactly one
//     identity dimension (see plan v5 Non-Negotiable).
//   - Per-(email, world) singleflight coalesces concurrent first-
//     call mints so a burst of tool calls for the same identity +
//     world produces one Issuer.MintFiltered round-trip, not N.
//   - Idle eviction: a session whose lastUsed predates
//     now - idleTimeout is evicted from the LRU tail on the next
//     Get (no background goroutine; piggybacks on access).
//   - LRU cap: when maxSessions is exceeded, the least-recently-
//     used session is evicted regardless of idle window. A
//     re-mint on the user's next call repopulates the cache —
//     that's the cost an evicted user pays.
//   - Raw tokens are NEVER persisted to disk; broker restart drops
//     everything.
//
// Locking: a single mu guards both the session map and the LRU
// list. Sessions are small (a handful of cached tokens per user)
// so the critical section is short; a sharded design is a
// Phase-7+ if a customer hits contention.
type sessionCache struct {
	mu          sync.Mutex
	sessions    map[string]*session
	lru         *list.List // *session in LRU order (front=newest)
	idleTimeout time.Duration
	maxSessions int
	clock       func() time.Time
	sf          singleflight.Group
}

// newSessionCache builds a sessionCache with the given LRU cap and
// idle timeout. clock is exposed so tests can pin time without
// sleeping; production wiring passes time.Now.
func newSessionCache(maxSessions int, idleTimeout time.Duration, clock func() time.Time) *sessionCache {
	if clock == nil {
		clock = time.Now
	}
	return &sessionCache{
		sessions:    make(map[string]*session, maxSessions),
		lru:         list.New(),
		idleTimeout: idleTimeout,
		maxSessions: maxSessions,
		clock:       clock,
	}
}

// errInvalidEmail surfaces when a caller passes an email that
// canonicalizes to "". Programming error: gatewayAuth already
// rejects empty-email tokens at the boundary, so the only way to
// reach this is a direct call from test code or a future caller
// that skips middleware. Returned (rather than panic) so the
// gateway handler can map it to an MCP tool-error envelope.
var errInvalidEmail = errors.New("broker: session cache: empty email key")

// mintFunc is the broker-side mint callback the cache invokes on
// first miss for a (email, world). Returns the raw token and its
// expiry; the cache stores the result and hands it back to the
// caller. Callers wire this to Issuer.MintFiltered narrowed to a
// single world via the keep predicate.
type mintFunc func(ctx context.Context) (cachedWorldToken, error)

// GetOrMint returns the cached world token for (email, worldName)
// or, on miss, calls mint to lazy-mint and caches the result.
// isFresh reports whether the returned token was just minted (true)
// or pulled from cache (false) — callers use this to gate the
// first-mint propagation-race retry on a subsequent dispatch 401.
//
// Cache hits update lastUsed (LRU bookkeeping). Concurrent first-
// call mints for the same (email, worldName) coalesce via
// singleflight: one mint round-trip, N callers receive the same
// result. Hash collisions are not possible because the key
// includes both components separated by a delimiter that cannot
// appear in either component (\x00 — invalid in email and world
// names alike).
func (c *sessionCache) GetOrMint(ctx context.Context, email, worldName string, mint mintFunc) (token cachedWorldToken, isFresh bool, err error) {
	key := canonicalEmail(email)
	if key == "" {
		return cachedWorldToken{}, false, errInvalidEmail
	}
	now := c.clock()

	c.mu.Lock()
	if hit, ok := c.lookupLocked(key, worldName, now); ok {
		c.mu.Unlock()
		return hit, false, nil
	}
	c.mu.Unlock()

	// Cache miss. Coalesce concurrent first-call mints for the
	// same (email, worldName); whichever goroutine wins the
	// singleflight does the Issuer round-trip, others wait for
	// its result. The key is delimiter-separated so an email
	// "alice@a.com" + world "b" cannot collide with email
	// "alice@a.comb" + world "" or any other splitting.
	sfKey := key + "\x00" + worldName
	v, sfErr, _ := c.sf.Do(sfKey, func() (any, error) {
		// Re-check after acquiring the singleflight slot in case
		// another goroutine populated the cache while we were
		// waiting. Saves one mint round-trip in the typical
		// burst-of-N-concurrent-calls pattern.
		now := c.clock()
		c.mu.Lock()
		if hit, ok := c.lookupLocked(key, worldName, now); ok {
			c.mu.Unlock()
			return mintOutcome{token: hit, fresh: false}, nil
		}
		c.mu.Unlock()

		minted, err := mint(ctx)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.storeLocked(key, worldName, minted, now)
		c.mu.Unlock()
		return mintOutcome{token: minted, fresh: true}, nil
	})
	if sfErr != nil {
		return cachedWorldToken{}, false, sfErr
	}
	out, ok := v.(mintOutcome)
	if !ok {
		// Defensive: singleflight returned the wrong shape. This
		// is unreachable given the closure above always returns
		// mintOutcome, but a future refactor could regress it
		// silently if we type-asserted without the ok check.
		return cachedWorldToken{}, false, errors.New("broker: session cache: internal type error")
	}
	return out.token, out.fresh, nil
}

// mintOutcome bundles the singleflight return so both the freshly-
// minted and the lost-the-race ("another goroutine populated the
// cache while I waited") paths return the same shape.
type mintOutcome struct {
	token cachedWorldToken
	fresh bool
}

// Invalidate drops the (email, worldName) cache entry. Called by
// the read-tool dispatcher after a world responds with
// `unauthorized` on a cache-hit token: the cache hit is wrong (the
// world's view of the token has changed under us, e.g. operator
// hand-revoked) so the next dispatch must re-mint. The session
// itself stays in the LRU even when its last world entry is
// dropped — a session with an empty worlds map still represents
// an authenticated identity that just doesn't have any valid
// cached tokens yet.
func (c *sessionCache) Invalidate(email, worldName string) {
	key := canonicalEmail(email)
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[key]
	if !ok {
		return
	}
	delete(s.worlds, worldName)
}

// Len returns the current number of cached sessions. Exposed for
// tests + future metrics; production code calls GetOrMint /
// Invalidate.
func (c *sessionCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

// lookupLocked returns the cached token if present and not
// expired, also advancing the session's LRU position and
// lastUsed. ok=false on cache miss (no session, no world entry, or
// expired entry). The caller holds c.mu.
func (c *sessionCache) lookupLocked(email, worldName string, now time.Time) (cachedWorldToken, bool) {
	c.evictIdleLocked(now)
	s, ok := c.sessions[email]
	if !ok {
		return cachedWorldToken{}, false
	}
	tok, ok := s.worlds[worldName]
	if !ok {
		return cachedWorldToken{}, false
	}
	if tok.expired(now) {
		delete(s.worlds, worldName)
		return cachedWorldToken{}, false
	}
	s.lastUsed = now
	c.lru.MoveToFront(s.lruElem)
	return tok, true
}

// storeLocked installs a freshly-minted token under (email,
// worldName). Creates the session on first write; enforces the
// LRU cap by evicting the tail when over. The caller holds c.mu.
func (c *sessionCache) storeLocked(email, worldName string, tok cachedWorldToken, now time.Time) {
	s, ok := c.sessions[email]
	if !ok {
		s = &session{
			email:    email,
			worlds:   make(map[string]cachedWorldToken, 4),
			lastUsed: now,
		}
		s.lruElem = c.lru.PushFront(s)
		c.sessions[email] = s
		c.evictOverCapLocked()
	} else {
		s.lastUsed = now
		c.lru.MoveToFront(s.lruElem)
	}
	s.worlds[worldName] = tok
}

// evictIdleLocked walks the LRU tail and drops sessions past the
// idle window. Idle eviction is piggybacked on access so the
// cache has no background goroutine to lifecycle; the cost of a
// scan is bounded by the number of stale entries between accesses
// (typically zero).
func (c *sessionCache) evictIdleLocked(now time.Time) {
	if c.idleTimeout <= 0 {
		return
	}
	cutoff := now.Add(-c.idleTimeout)
	for c.lru.Len() > 0 {
		elem := c.lru.Back()
		s, ok := elem.Value.(*session)
		if !ok || s.lastUsed.After(cutoff) {
			return
		}
		c.lru.Remove(elem)
		delete(c.sessions, s.email)
	}
}

// evictOverCapLocked drops the LRU tail until the session count
// fits under maxSessions. Called after a new-session insert so
// the cap is enforced at-most-one-over (the new session never
// causes itself to be evicted because PushFront put it at the
// head). The caller holds c.mu.
func (c *sessionCache) evictOverCapLocked() {
	for c.maxSessions > 0 && c.lru.Len() > c.maxSessions {
		elem := c.lru.Back()
		s, ok := elem.Value.(*session)
		if !ok {
			return
		}
		c.lru.Remove(elem)
		delete(c.sessions, s.email)
	}
}
