// Package ratelimit provides per-IP request rate limiting.
package ratelimit

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // UnixNano timestamp
}

// Limiter tracks per-IP request rates using a token bucket algorithm.
type Limiter struct {
	rate         rate.Limit
	burst        int
	cleanupEvery time.Duration // how often the cleanup goroutine runs
	staleAfter   time.Duration // evict entries idle longer than this
	ips          sync.Map      // map[string]*entry

	stop chan struct{}
}

// New creates a Limiter that allows r requests per second with the given burst size.
// A background goroutine evicts stale entries every 60 seconds.
// Call Stop to release resources.
func New(r float64, burst int) *Limiter {
	return NewWithCleanup(r, burst, 60*time.Second, 5*time.Minute)
}

// NewWithCleanup is like New but allows configuring the cleanup interval and stale threshold.
func NewWithCleanup(r float64, burst int, cleanupEvery, staleAfter time.Duration) *Limiter {
	l := &Limiter{
		rate:         rate.Limit(r),
		burst:        burst,
		cleanupEvery: cleanupEvery,
		staleAfter:   staleAfter,
		stop:         make(chan struct{}),
	}
	go l.cleanup()
	return l
}

// Allow reports whether a request from the given IP should be permitted.
func (l *Limiter) Allow(ip string) bool {
	now := time.Now().UnixNano()

	// Fast path: reuse existing entry without allocating a new limiter.
	if v, ok := l.ips.Load(ip); ok {
		e := v.(*entry)
		e.lastSeen.Store(now)
		return e.limiter.Allow()
	}

	// Slow path: create entry and attempt to store it.
	e := &entry{limiter: rate.NewLimiter(l.rate, l.burst)}
	e.lastSeen.Store(now)
	v, _ := l.ips.LoadOrStore(ip, e)
	actual := v.(*entry)
	actual.lastSeen.Store(now)
	return actual.limiter.Allow()
}

// Stop terminates the background cleanup goroutine.
func (l *Limiter) Stop() {
	close(l.stop)
}

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(l.cleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-ticker.C:
			cutoff := now.Add(-l.staleAfter).UnixNano()
			l.ips.Range(func(key, value any) bool {
				e := value.(*entry)
				if e.lastSeen.Load() < cutoff {
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
