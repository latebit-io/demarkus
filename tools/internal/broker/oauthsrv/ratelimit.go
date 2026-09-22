package oauthsrv

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/tools/internal/broker/core"
)

// requireAuth verifies the bearer, gates its domain and puts the claims on
// the context for SubjectRateLimit; any failure is a plain 401 here.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := core.BearerToken(r)
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
		if !core.OIDCDomainAllowed(s.cfg.OIDC.AllowDomains, claims.HD) {
			s.log.InfoContext(r.Context(), "broker: bearer rejected by allowDomains",
				"subject", core.HashSubject(claims.Subject), "hd", claims.HD)
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(core.CtxWithClaims(r.Context(), &claims)))
	})
}

// ipRateLimit limits unauthenticated routes (/auth/login) per client IP;
// nil loginReg passes through.
func (s *Server) ipRateLimit(next http.Handler) http.Handler {
	if s.loginReg == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.clientIP(r)
		allowed, retryAfter := s.loginReg.Reserve(key)
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

// clientIP keys the IP limiter: the leftmost X-Forwarded-For entry when
// trustForwardedFor is on, else the peer. Trusting the header behind no
// proxy lets a client rotate synthetic IPs past the limiter.
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
	// An unparsable RemoteAddr (tests) keys on the raw string.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
