package broker

import (
	"context"
	"embed"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// Soul template seeding: a tenant world's first authorized call
// publishes the soul layout (hub, policy, template). Create-only
// publishes make it idempotent across replicas; conflict = already seeded.

//go:embed soulseed/*.md
var soulSeedFS embed.FS

// soulSeedDoc maps one embedded seed to its destination and catalog
// metadata. The agent key marks broker-initiated writes in the world's
// audit trail (user writes carry the caller's email instead).
type soulSeedDoc struct {
	embedName string
	path      string
	meta      map[string]string
}

// soulSeedDocs is ordered: /index.md last, because its existence is the
// seeded-check sentinel and must only appear once the other documents do.
func soulSeedDocs() []soulSeedDoc {
	const agent = "demarkus-memory-broker"
	return []soulSeedDoc{
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
				"agent": agent, "tags": "template,layout,soul,routes",
				"importance": "0.7", "type": "Reference",
			},
		},
		{
			embedName: "index.md",
			path:      "/index.md",
			meta: map[string]string{
				"agent": agent, "tags": "index,hub,soul,navigation",
				"importance": "0.9", // hubs stay untyped
			},
		},
	}
}

// soulSeedRetryInterval throttles re-attempts after a failed seed, so an
// unhealthy world does not add seed round trips to every tool call.
const soulSeedRetryInterval = time.Minute

// soulSeeder tracks per-world seeding state for one gateway pod.
// Single-flight so concurrent first calls seed once; waiters block until
// the seeding attempt finishes so their own reads see the seeded world.
type soulSeeder struct {
	mu       sync.Mutex
	seeded   map[string]bool
	inflight map[string]chan struct{}
	failedAt map[string]time.Time
}

// ensureSoulSeed makes sure w carries the soul template, seeding it when
// absent. Best-effort: failures warn and the tool call proceeds; the next
// call retries because only a verified seed marks the world done.
func (g *mcpGateway) ensureSoulSeed(ctx context.Context, w *WorldConfig) {
	s := &g.soulSeed
	s.mu.Lock()
	if s.seeded[w.Name] {
		s.mu.Unlock()
		return
	}
	if done := s.inflight[w.Name]; done != nil {
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	// Failure throttle: don't re-attempt against an unhealthy world on
	// every tool call (same shape as seedCheckInterval for graph seeds).
	if last, failed := s.failedAt[w.Name]; failed && time.Since(last) < soulSeedRetryInterval {
		s.mu.Unlock()
		return
	}
	if s.seeded == nil {
		s.seeded = make(map[string]bool)
	}
	if s.inflight == nil {
		s.inflight = make(map[string]chan struct{})
	}
	if s.failedAt == nil {
		s.failedAt = make(map[string]time.Time)
	}
	done := make(chan struct{})
	s.inflight[w.Name] = done
	s.mu.Unlock()

	ok := g.seedSoulWorld(ctx, w)

	s.mu.Lock()
	if ok {
		s.seeded[w.Name] = true
		delete(s.failedAt, w.Name)
	} else {
		s.failedAt[w.Name] = time.Now()
	}
	close(done)
	delete(s.inflight, w.Name)
	s.mu.Unlock()
}

// seedSoulWorld checks for the /index.md sentinel and publishes the seed
// set when the world is empty. Returns true when the world is verified
// seeded (already or now); false keeps the world eligible for retry.
func (g *mcpGateway) seedSoulWorld(ctx context.Context, w *WorldConfig) bool {
	result, err := g.dispatcher.FetchContext(ctx, w.Name, "/index.md", "")
	if err != nil {
		g.log.Warn("soul seed check failed", "world", w.Name, "err", err)
		return false
	}
	switch result.Response.Status {
	case protocol.StatusOK, protocol.StatusArchived:
		return true // already seeded (archived counts as a deliberate act)
	case protocol.StatusNotFound:
		// fresh world; seed below
	default:
		g.log.Warn("soul seed check returned unexpected status", "world", w.Name, "status", result.Response.Status)
		return false
	}
	for _, doc := range soulSeedDocs() {
		body, readErr := soulSeedFS.ReadFile("soulseed/" + doc.embedName)
		if readErr != nil {
			// Broken embed is a build defect; surface loudly but keep
			// the tool call alive.
			g.log.Error("soul seed embed unreadable", "name", doc.embedName, "err", readErr)
			return false
		}
		// dispatchWithWriteAuth provisions the world write token and
		// absorbs first-mint Secret propagation lag; a fresh tenant
		// world's very first write is exactly that case.
		pres, pubErr := g.dispatchWithWriteAuth(ctx, w.Name, func(token string) (fetch.Result, error) {
			return g.dispatcher.Publish(w.Name, doc.path, string(body), token, 0, doc.meta)
		})
		if pubErr != nil {
			g.log.Warn("soul seed publish failed", "world", w.Name, "path", doc.path, "err", pubErr)
			return false
		}
		switch pres.Response.Status {
		case protocol.StatusCreated, protocol.StatusOK, protocol.StatusConflict:
			// conflict = another replica or the user won the race; fine
		default:
			g.log.Warn("soul seed publish returned unexpected status", "world", w.Name, "path", doc.path, "status", pres.Response.Status)
			return false
		}
	}
	g.log.Info("soul template seeded", "world", w.Name)
	return true
}
