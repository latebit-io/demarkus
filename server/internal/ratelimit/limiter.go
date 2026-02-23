// Package ratelimit provides per-IP request rate limiting.
package ratelimit

import (
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter tracks per-IP request rates using a token bucket algorithm.
type Limiter struct {
	rate  rate.Limit
	burst int
	ips   sync.Map // map[string]*entry

	stop chan struct{}
}

// New creates a Limiter that allows r requests per second with the given burst size.
// A background goroutine evicts stale entries every 60 seconds.
// Call Stop to release resources.
func New(r float64, burst int) *Limiter {
	l := &Limiter{
		rate:  rate.Limit(r),
		burst: burst,
		stop:  make(chan struct{}),
	}
	go l.cleanup()
	return l
}

// Allow reports whether a request from the given IP should be permitted.
func (l *Limiter) Allow(ip string) bool {
	now := time.Now()
	v, _ := l.ips.LoadOrStore(ip, &entry{
		limiter:  rate.NewLimiter(l.rate, l.burst),
		lastSeen: now,
	})
	e := v.(*entry)
	e.lastSeen = now
	return e.limiter.Allow()
}

// Stop terminates the background cleanup goroutine.
func (l *Limiter) Stop() {
	close(l.stop)
}

const staleAfter = 5 * time.Minute

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-ticker.C:
			l.ips.Range(func(key, value any) bool {
				e := value.(*entry)
				if now.Sub(e.lastSeen) > staleAfter {
					l.ips.Delete(key)
				}
				return true
			})
		}
	}
}

// ExtractIP returns the IP portion of a net.Addr (strips the port).
func ExtractIP(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
