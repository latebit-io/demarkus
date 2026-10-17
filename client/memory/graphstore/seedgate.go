package graphstore

import (
	"context"
	"sync"
	"time"
)

// SeedCheckInterval caps seed checks at one per owner per interval, success
// or failure; the first call after start always checks.
const SeedCheckInterval = 5 * time.Minute

// SeedGate single-flights seed passes per owner under SeedTimeout and backs
// off repeat checks. Zero value is ready. Shared by every MCP surface.
type SeedGate struct {
	mu         sync.Mutex
	checked    map[string]time.Time
	refreshing map[string]chan struct{}
}

// Run executes pass unless a check ran within the interval. Concurrent callers
// wait for the in-flight pass. A caller cancel never counts as a check.
func (g *SeedGate) Run(ctx context.Context, owner string, pass func(ctx context.Context) bool) {
	if ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	if done := g.refreshing[owner]; done != nil {
		g.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	if last, ok := g.checked[owner]; ok && time.Since(last) < SeedCheckInterval {
		g.mu.Unlock()
		return
	}
	if g.checked == nil {
		g.checked = make(map[string]time.Time)
	}
	if g.refreshing == nil {
		g.refreshing = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	g.refreshing[owner] = done
	g.mu.Unlock()

	callerCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, SeedTimeout)
	defer cancel()
	defer func() {
		g.mu.Lock()
		if callerCtx.Err() == nil {
			g.checked[owner] = time.Now()
		}
		close(done)
		delete(g.refreshing, owner)
		g.mu.Unlock()
	}()
	pass(ctx)
}

// LastCheck reports when owner was last checked, if ever.
func (g *SeedGate) LastCheck(owner string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	last, ok := g.checked[owner]
	return last, ok
}

// Expire forgets owner's last check so the next Run checks again.
func (g *SeedGate) Expire(owner string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.checked, owner)
}
