package core

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// minSweepInterval bounds how often a registry scans for idle limiters.
const minSweepInterval = time.Minute

// RateLimitRegistry keeps one limiter per key (subject hash or client
// address), per replica. A limiter idle long enough to have refilled is
// dropped: a fresh one behaves the same, so memory tracks recent keys only.
type RateLimitRegistry struct {
	perMinute int
	burst     int
	// refill is how long an exhausted bucket takes to fill again.
	refill time.Duration
	now    func() time.Time

	mu       sync.Mutex
	limiters map[string]*keyLimiter
	swept    time.Time
}

type keyLimiter struct {
	limiter *rate.Limiter
	used    time.Time
}

// newRateLimitRegistry is nil for a non positive rate or burst; the
// middleware treats nil as no enforcement.
func newRateLimitRegistry(perMinute, burst int) *RateLimitRegistry {
	if perMinute <= 0 || burst <= 0 {
		return nil
	}
	refill := time.Duration(float64(burst) / float64(perMinute) * float64(time.Minute))
	return &RateLimitRegistry{
		perMinute: perMinute, burst: burst, refill: refill, now: time.Now,
		limiters: make(map[string]*keyLimiter),
	}
}

// limiter is the key's bucket, allocated on first use; the caller holds r.mu.
func (r *RateLimitRegistry) limiter(key string, now time.Time) *rate.Limiter {
	r.sweep(now)
	entry, ok := r.limiters[key]
	if !ok {
		// rate.Limit is events per second; the operator's knob is per minute.
		entry = &keyLimiter{limiter: rate.NewLimiter(rate.Limit(float64(r.perMinute)/60.0), r.burst)}
		r.limiters[key] = entry
	}
	entry.used = now
	return entry.limiter
}

// sweep drops every limiter idle for a full refill, at most once per
// interval; the caller holds r.mu.
func (r *RateLimitRegistry) sweep(now time.Time) {
	if now.Sub(r.swept) < max(r.refill, minSweepInterval) {
		return
	}
	r.swept = now
	for key, entry := range r.limiters {
		if now.Sub(entry.used) >= r.refill {
			delete(r.limiters, key)
		}
	}
}

// Reserve charges key's bucket for one event. A denial reports the wait
// (at least 1s, so Retry-After is never 0) and, through Reserve plus
// Cancel, consumes no budget: hammering never pushes recovery out.
func (r *RateLimitRegistry) Reserve(key string) (allowed bool, retryAfter time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	res := r.limiter(key, now).ReserveN(now, 1)
	if !res.OK() {
		// Unreachable without a max future reservation; deny with the minimum hint.
		return false, time.Second
	}
	if d := res.DelayFrom(now); d > 0 {
		res.CancelAt(now)
		return false, max(d, time.Second)
	}
	return true, 0
}

// NewRateLimits builds the two registries from config; nil when the limiter
// is disabled or a rate is zero, and the middleware then passes through.
func NewRateLimits(cfg *RateLimitConfig) (subject, login *RateLimitRegistry) {
	if cfg.Disabled {
		return nil, nil
	}
	return newRateLimitRegistry(cfg.Tokens.PerMinute, cfg.Tokens.Burst),
		newRateLimitRegistry(cfg.Login.PerMinute, cfg.Login.Burst)
}
