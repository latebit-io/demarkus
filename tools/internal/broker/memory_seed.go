package broker

import (
	"context"
	"embed"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// Memory template seeding: a tenant world's first authorized call
// publishes the memory layout (hub, policy, template). Create-only
// publishes make it idempotent across replicas; conflict = already seeded.

//go:embed memoryseed/*.md
var memorySeedFS embed.FS

// memorySeedDoc maps one embedded seed to its destination and catalog
// metadata. The agent key marks broker-initiated writes in the world's
// audit trail (user writes carry the caller's email instead).
type memorySeedDoc struct {
	embedName string
	path      string
	meta      map[string]string
}

// memorySeedDocs is ordered: /index.md last, because its existence is the
// seeded-check sentinel and must only appear once the other documents do.
func memorySeedDocs() []memorySeedDoc {
	const agent = "demarkus-memory-broker"
	return []memorySeedDoc{
		{
			embedName: "policy.md",
			path:      "/.well-known/demarkus/policy.md",
			meta: map[string]string{
				"agent": agent, "tags": "policy,write-policy,style,metadata",
				"importance": "0.7", "type": "Reference",
			},
		},
		{
			embedName: "template.md",
			path:      "/.well-known/demarkus/template.md",
			meta: map[string]string{
				"agent": agent, "tags": "template,layout,memory,routes",
				"importance": "0.7", "type": "Reference",
			},
		},
		{
			embedName: "index.md",
			path:      "/index.md",
			meta: map[string]string{
				"agent": agent, "tags": "index,hub,memory,navigation",
				"importance": "0.9", // hubs stay untyped
			},
		},
	}
}

// memorySeedRetryInterval throttles re-attempts after a failed seed, so an
// unhealthy world does not add seed round trips to every tool call.
const memorySeedRetryInterval = time.Minute

// memorySeeder tracks per-world seeding state for one gateway pod.
// Single-flight so concurrent first calls seed once; waiters block until
// the seeding attempt finishes so their own reads see the seeded world.
type memorySeeder struct {
	mu     sync.Mutex
	worlds map[string]*memorySeedState
}

// memorySeedState is one world's seeding lifecycle: done, in flight, or
// throttled after a failure.
type memorySeedState struct {
	seeded   bool
	inflight chan struct{}
	failedAt time.Time
}

func (s *memorySeeder) stateFor(world string) *memorySeedState {
	if s.worlds == nil {
		s.worlds = make(map[string]*memorySeedState)
	}
	state, ok := s.worlds[world]
	if !ok {
		state = &memorySeedState{}
		s.worlds[world] = state
	}
	return state
}

// ensureMemorySeed makes sure w carries the memory template, seeding it when
// absent. Best-effort: failures warn and the tool call proceeds; the next
// call retries because only a verified seed marks the world done.
func (g *mcpGateway) ensureMemorySeed(ctx context.Context, w *WorldConfig) {
	s := &g.memorySeed
	s.mu.Lock()
	state := s.stateFor(w.Name)
	if state.seeded {
		s.mu.Unlock()
		return
	}
	if state.inflight != nil {
		done := state.inflight
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	// Failure throttle: don't re-attempt against an unhealthy world on
	// every tool call (same shape as seedCheckInterval for graph seeds).
	if !state.failedAt.IsZero() && time.Since(state.failedAt) < memorySeedRetryInterval {
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	state.inflight = done
	s.mu.Unlock()

	ok := g.seedMemoryWorld(ctx, w)

	s.mu.Lock()
	state.seeded = ok
	if ok {
		state.failedAt = time.Time{}
	} else {
		state.failedAt = time.Now()
	}
	state.inflight = nil
	close(done)
	s.mu.Unlock()
}

// seedMemoryWorld checks for the /index.md sentinel and publishes the seed
// set when the world is empty. Returns true when the world is verified
// seeded (already or now); false keeps the world eligible for retry.
func (g *mcpGateway) seedMemoryWorld(ctx context.Context, w *WorldConfig) bool {
	result, err := g.dispatcher.FetchContext(ctx, w.Name, "/index.md", "")
	if err != nil {
		g.log.Warn("memory seed check failed", "world", w.Name, "err", err)
		return false
	}
	switch result.Response.Status {
	case protocol.StatusOK, protocol.StatusArchived:
		return true // already seeded (archived counts as a deliberate act)
	case protocol.StatusNotFound:
		// fresh world; seed below
	default:
		g.log.Warn("memory seed check returned unexpected status", "world", w.Name, "status", result.Response.Status)
		return false
	}
	for _, doc := range memorySeedDocs() {
		body, readErr := memorySeedFS.ReadFile("memoryseed/" + doc.embedName)
		if readErr != nil {
			// Broken embed is a build defect; surface loudly but keep
			// the tool call alive.
			g.log.Error("memory seed embed unreadable", "name", doc.embedName, "err", readErr)
			return false
		}
		// dispatchWithWriteAuth provisions the world write token and
		// absorbs first-mint Secret propagation lag; a fresh tenant
		// world's very first write is exactly that case.
		pres, pubErr := g.dispatchWithWriteAuth(ctx, w.Name, func(token string) (fetch.Result, error) {
			return g.dispatcher.Publish(w.Name, doc.path, string(body), token, 0, doc.meta)
		})
		if pubErr != nil {
			g.log.Warn("memory seed publish failed", "world", w.Name, "path", doc.path, "err", pubErr)
			return false
		}
		switch pres.Response.Status {
		case protocol.StatusCreated, protocol.StatusOK, protocol.StatusConflict:
			// conflict = another replica or the user won the race; fine
		default:
			g.log.Warn("memory seed publish returned unexpected status", "world", w.Name, "path", doc.path, "status", pres.Response.Status)
			return false
		}
	}
	g.log.Info("memory template seeded", "world", w.Name)
	return true
}
