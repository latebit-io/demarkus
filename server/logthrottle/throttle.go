// Package logthrottle bounds how often a repeated event is logged, so a
// flood of refusals costs one line per window instead of one per request.
package logthrottle

import (
	"sync"
	"time"
)

// DefaultWindow is the refusal-log window the limiters use.
const DefaultWindow = time.Minute

// Throttle admits the first occurrence at once and one per window after it.
// It holds a timestamp and a count, so its size never grows with traffic.
type Throttle struct {
	window time.Duration
	now    func() time.Time

	mu         sync.Mutex
	lastLogged time.Time
	suppressed int64
}

// New returns a Throttle that logs at most once per window.
func New(window time.Duration) *Throttle {
	return &Throttle{window: window, now: time.Now}
}

// Allow reports whether this occurrence should be logged and, if so, how
// many were held back since the previous logged one. The count of a final
// window rides on the next logged occurrence. A nil Throttle logs every one.
func (t *Throttle) Allow() (log bool, suppressed int64) {
	if t == nil {
		return true, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.lastLogged.IsZero() && now.Sub(t.lastLogged) < t.window {
		t.suppressed++
		return false, 0
	}
	suppressed = t.suppressed
	t.lastLogged, t.suppressed = now, 0
	return true, suppressed
}
