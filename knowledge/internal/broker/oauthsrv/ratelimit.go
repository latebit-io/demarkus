package oauthsrv

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/httpauth"
	"github.com/latebit-io/demarkus/server/logthrottle"
)

// requireAuth verifies the bearer, gates its domain and puts the claims on
// the context for SubjectRateLimit; any failure is a plain 401 here.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := httpauth.BearerToken(r)
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
	refusals := logthrottle.New(logthrottle.DefaultWindow)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.clientIP(r)
		allowed, retryAfter := s.loginReg.Reserve(key)
		if !allowed {
			if logIt, suppressed := refusals.Allow(); logIt {
				s.log.WarnContext(r.Context(), "broker: rate limit exceeded",
					"route", r.URL.Path, "ip", key, "retryAfter", retryAfter, "suppressed", suppressed)
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP keys the IP limiter: the leftmost X-Forwarded-For entry when
// trustForwardedFor is on and it is an address, else the peer. Trusting the
// header behind no proxy lets a client rotate synthetic IPs past the limiter.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustForwardedFor {
		// Leftmost non-empty entry; one that is no address never becomes a key.
		for part := range strings.SplitSeq(r.Header.Get("X-Forwarded-For"), ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			if key, ok := addressKey(part); ok {
				return key
			}
			break
		}
	}
	// An unparsable RemoteAddr (tests) keys on the raw string.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if key, ok := addressKey(host); ok {
		return key
	}
	return host
}

// addressKey is the limiter key of an address: an IPv4 address itself, an
// IPv6 address's /64, since one subscriber holds a whole /64 to rotate through.
func addressKey(raw string) (string, bool) {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return "", false
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String(), true
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return "", false
	}
	return prefix.String(), true
}
