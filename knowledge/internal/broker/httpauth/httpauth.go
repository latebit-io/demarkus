// Package httpauth is the HTTP edge both broker listeners mount: reading the
// bearer off a request and limiting an authenticated route per subject.
package httpauth

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/server/logthrottle"
)

// BearerToken is the Authorization bearer, or "" when absent or malformed.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	// RFC 6750 §2.1: the scheme is case-insensitive.
	parts := strings.Fields(h)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// SubjectRateLimit limits an authenticated route per subject, so rotating
// IPs or several sessions of one identity share a bucket. It runs after the
// auth middleware and fails closed when the claims are missing.
func SubjectRateLimit(reg *core.RateLimitRegistry, log *slog.Logger, next http.Handler) http.Handler {
	if reg == nil {
		return next
	}
	refusals := logthrottle.New(logthrottle.DefaultWindow)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := core.ClaimsFromCtx(r.Context())
		if !ok {
			log.ErrorContext(r.Context(), "broker: subjectRateLimit missing claims in context; middleware miswired")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		key := core.HashSubject(claims.Subject)
		allowed, retryAfter := reg.Reserve(key)
		if !allowed {
			// A flood logs once per window with the held-back count, not per refusal.
			if logIt, suppressed := refusals.Allow(); logIt {
				log.WarnContext(r.Context(), "broker: rate limit exceeded",
					"route", r.URL.Path, "subject", key, "retryAfter", retryAfter, "suppressed", suppressed)
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
