package broker

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimitRegistry keeps one *rate.Limiter per key (subject hash or client
// IP). Per replica and without eviction by design; the sizing and the
// trustForwardedFor caveat are in docs/site/deployment/kubernetes.md.
type rateLimitRegistry struct {
	mu        sync.Map // key string → *rate.Limiter
	perMinute int
	burst     int
}

// newRateLimitRegistry returns nil when the configured rate is non-
// positive or the burst is zero/negative; the middleware reads "nil
// registry → no enforcement" so a test or operator can disable a
// specific dimension without threading a bool through every handler.
func newRateLimitRegistry(perMinute, burst int) *rateLimitRegistry {
	if perMinute <= 0 || burst <= 0 {
		return nil
	}
	return &rateLimitRegistry{perMinute: perMinute, burst: burst}
}

// limiter returns the *rate.Limiter for a given key, creating it on
// first access. The double-check via LoadOrStore is the standard
// sync.Map "compute once" pattern: most reads find the existing entry,
// only the very first request for a key allocates.
func (r *rateLimitRegistry) limiter(key string) *rate.Limiter {
	if v, ok := r.mu.Load(key); ok {
		return v.(*rate.Limiter)
	}
	// rate.Limit accepts events/sec; we configure events/min so the
	// operator-facing knob matches the human-scale rates in the plan.
	lim := rate.NewLimiter(rate.Limit(float64(r.perMinute)/60.0), r.burst)
	actual, _ := r.mu.LoadOrStore(key, lim)
	return actual.(*rate.Limiter)
}

// reserve charges the bucket for one event keyed by key. Returns
// allowed=true when the request may proceed immediately; false when it
// should be rejected. retryAfter is set on denial as the duration until
// the next token would be available (clamped to at least 1s so the
// Retry-After header is never 0 — a 0 means "try again immediately"
// which an aggressive client will interpret as "spam harder").
//
// We use Reserve()+Cancel() rather than Allow() so a denied request
// does NOT consume budget: an attacker hammering the endpoint stays
// stuck at the same retry window rather than pushing the recovery
// indefinitely into the future. With Allow(), each denied attempt
// would internally take a token and lengthen recovery.
func (r *rateLimitRegistry) reserve(key string) (allowed bool, retryAfter time.Duration) {
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

// requireAuth verifies the bearer ID token on the request, stashes the
// verified Claims on r.Context() under ctxClaimsKey, and calls the next
// handler. On any failure it writes the 401 itself and short-circuits —
// the next handler is not called.
//
// Extracted from the per-handler s.authenticate calls in Slice C.4 so
// the subjectRateLimit middleware can key on hashSubject(claims.Subject)
// without re-verifying the bearer.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			http.Error(w, "Authorization: Bearer <id_token> required", http.StatusUnauthorized)
			return
		}
		claims, err := s.verifier.VerifyIDToken(r.Context(), raw)
		if err != nil {
			s.log.WarnContext(r.Context(), "broker: id_token verification failed", "err", err)
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		if !oidcDomainAllowed(s.cfg.OIDC.AllowDomains, claims.HD) {
			s.log.InfoContext(r.Context(), "broker: bearer rejected by allowDomains",
				"subject", hashSubject(claims.Subject), "hd", claims.HD)
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctxWithClaims(r.Context(), &claims)))
	})
}

// newRateLimits builds the two registries from config; nil when the limiter
// is disabled or a rate is zero, and the middleware then passes through.
func newRateLimits(cfg *RateLimitConfig) (subject, login *rateLimitRegistry) {
	if cfg.Disabled {
		return nil, nil
	}
	return newRateLimitRegistry(cfg.Tokens.PerMinute, cfg.Tokens.Burst),
		newRateLimitRegistry(cfg.Login.PerMinute, cfg.Login.Burst)
}

// subjectRateLimit limits an authenticated route per subject, so rotating
// IPs or several sessions of one identity share a bucket. It runs after the
// auth middleware and fails closed when the claims are missing.
func subjectRateLimit(reg *rateLimitRegistry, log *slog.Logger, next http.Handler) http.Handler {
	if reg == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := claimsFromCtx(r.Context())
		if !ok {
			log.ErrorContext(r.Context(), "broker: subjectRateLimit missing claims in context; middleware miswired")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		key := hashSubject(claims.Subject)
		allowed, retryAfter := reg.reserve(key)
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

// ipRateLimit enforces source-IP rate limiting on unauthenticated routes
// — primarily /auth/login so an unauthenticated client cannot exhaust
// the OIDC dance machinery by spamming logins. Keys on s.clientIP(r)
// which respects the trustForwardedFor flag.
//
// Returns next unchanged when s.loginReg is nil so the disabled state
// has no per-request overhead.
func (s *Server) ipRateLimit(next http.Handler) http.Handler {
	if s.loginReg == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.clientIP(r)
		allowed, retryAfter := s.loginReg.reserve(key)
		if !allowed {
			s.log.WarnContext(r.Context(), "broker: rate limit exceeded",
				"route", r.URL.Path, "ip", key, "retryAfter", retryAfter)
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the rate-limit key for IP-keyed routes. When
// trustForwardedFor is enabled, uses the leftmost entry in X-Forwarded-
// For (the standard "originating client" semantic for reverse proxies).
// Otherwise uses the immediate peer from r.RemoteAddr.
//
// trustForwardedFor must stay OFF unless the broker is actually behind
// a proxy that strips or appends to XFF correctly — otherwise an
// attacker can spoof the header and bypass the per-IP limiter by
// rotating through synthetic IPs. The chart-side §6.3 work will wire
// this flag on when the broker sits behind the Ingress.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Leftmost non-empty entry. Trim whitespace because
			// proxies commonly emit "ip1, ip2, ip3" with spaces.
			for part := range strings.SplitSeq(xff, ",") {
				part = strings.TrimSpace(part)
				if part != "" {
					return part
				}
			}
		}
	}
	// SplitHostPort handles both "host:port" (real HTTP requests) and
	// "[v6]:port" forms. On the rare error case (e.g. tests that
	// inject an invalid RemoteAddr) fall back to the raw string so
	// the limiter still groups consistently.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
