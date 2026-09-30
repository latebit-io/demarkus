package core

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitRegistry keeps one *rate.Limiter per key (subject hash or client
// IP). Per replica and without eviction by design; the sizing and the
// trustForwardedFor caveat are in docs/site/deployment/kubernetes.md.
type RateLimitRegistry struct {
	mu        sync.Map // key string → *rate.Limiter
	perMinute int
	burst     int
}

// newRateLimitRegistry is nil for a non positive rate or burst; the
// middleware treats nil as no enforcement.
func newRateLimitRegistry(perMinute, burst int) *RateLimitRegistry {
	if perMinute <= 0 || burst <= 0 {
		return nil
	}
	return &RateLimitRegistry{perMinute: perMinute, burst: burst}
}

// limiter is the key's bucket, allocated on first use.
func (r *RateLimitRegistry) limiter(key string) *rate.Limiter {
	if v, ok := r.mu.Load(key); ok {
		return v.(*rate.Limiter)
	}
	// rate.Limit accepts events/sec; we configure events/min so the
	// operator-facing knob matches the human-scale rates in the plan.
	lim := rate.NewLimiter(rate.Limit(float64(r.perMinute)/60.0), r.burst)
	actual, _ := r.mu.LoadOrStore(key, lim)
	return actual.(*rate.Limiter)
}

// Reserve charges key's bucket for one event. A denial reports the wait
// (at least 1s, so Retry-After is never 0) and, through Reserve plus
// Cancel, consumes no budget: hammering never pushes recovery out.
func (r *RateLimitRegistry) Reserve(key string) (allowed bool, retryAfter time.Duration) {
	lim := r.limiter(key)
	res := lim.Reserve()
	if !res.OK() {
		// Practically unreachable since we don't set a max-future-
		// reservation, but if it ever does happen treat it as denial
		// with a 1s retry (the minimum nonzero hint).
		return false, time.Second
	}
	if d := res.Delay(); d > 0 {
		res.Cancel()
		if d < time.Second {
			d = time.Second
		}
		return false, d
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

// SubjectRateLimit limits an authenticated route per subject, so rotating
// IPs or several sessions of one identity share a bucket. It runs after the
// auth middleware and fails closed when the claims are missing.
func SubjectRateLimit(reg *RateLimitRegistry, log *slog.Logger, next http.Handler) http.Handler {
	if reg == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromCtx(r.Context())
		if !ok {
			log.ErrorContext(r.Context(), "broker: subjectRateLimit missing claims in context; middleware miswired")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		key := HashSubject(claims.Subject)
		allowed, retryAfter := reg.Reserve(key)
		if !allowed {
			log.WarnContext(r.Context(), "broker: rate limit exceeded",
				"route", r.URL.Path, "subject", key, "retryAfter", retryAfter)
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
