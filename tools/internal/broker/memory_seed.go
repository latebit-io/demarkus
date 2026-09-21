package broker

import (
	"context"
	"embed"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// Memory template seeding: a tenant world's first authorized call publishes
// the memory layout (hub, policy, template). Version-checked publishes make it
// idempotent across replicas; conflict = already seeded.

//go:embed memoryseed/*.md
var memorySeedFS embed.FS

// memorySeedDoc maps one embedded seed to its destination and catalog
// metadata. The agent key marks broker-initiated writes in the world's
// audit trail (user writes carry the caller's email instead).
type memorySeedDoc struct {
	embedName string
	path      string
	meta      map[string]string
	// overServerSeed publishes as version two when the server's own marked
	// seed holds version one; any other existing document is left alone.
	overServerSeed bool
}

// memorySeedDocs is ordered: /index.md last, because its existence is the
// seeded-check sentinel and must only appear once the other documents do.
func memorySeedDocs() []memorySeedDoc {
	const agent = "demarkus-memory-broker"
	return []memorySeedDoc{
		{
			embedName:      "policy.md",
			path:           publishpolicy.DocumentPath,
			overServerSeed: true,
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

// forget drops a world's seeding state: a world of that name provisioned later
// is a new, empty one. A seed still in flight finishes on its orphaned state.
func (s *memorySeeder) forget(world string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.worlds, world)
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

// seedMemoryWorld seeds a world that lacks the /index.md sentinel; one that has
// it is still checked for a server policy seed an older broker left in place.
// True means verified seeded; false keeps the world eligible for retry.
func (g *mcpGateway) seedMemoryWorld(ctx context.Context, w *WorldConfig) bool {
	result, err := g.dispatcher.Fetch(ctx, fetch.FetchRequest{Host: w.Name, Path: "/index.md"})
	if err != nil {
		g.log.Warn("memory seed check failed", "world", w.Name, "err", err)
		return false
	}
	seeded := false
	switch result.Response.Status {
	case protocol.StatusOK:
		seeded = true
	case protocol.StatusArchived:
		return true // archived counts as a deliberate act
	case protocol.StatusNotFound:
		// fresh world; seed below
	default:
		g.log.Warn("memory seed check returned unexpected status", "world", w.Name, "status", result.Response.Status)
		return false
	}
	for _, doc := range memorySeedDocs() {
		if seeded && !doc.overServerSeed {
			continue
		}
		if !g.publishMemorySeedDoc(ctx, w, &doc, seeded) {
			return false
		}
	}
	if !seeded {
		g.log.Info("memory template seeded", "world", w.Name)
	}
	return true
}

// seedAction is what to do with one seed document.
type seedAction int

const (
	seedFailed  seedAction = iota // the check could not run; retry later
	seedCreate                    // publish create-only
	seedReplace                   // publish over the server's marked seed
	seedKeep                      // something else holds the path; leave it
)

// expectedVersion is the version a publish for this action commits against.
func (a seedAction) expectedVersion() int {
	if a == seedReplace {
		return 1
	}
	return 0
}

// memorySeedAction decides one seed publish. Only a version one carrying the
// server's marker is replaced; a server that does not seed leaves not found.
func (g *mcpGateway) memorySeedAction(ctx context.Context, w *WorldConfig, doc *memorySeedDoc) seedAction {
	if !doc.overServerSeed {
		return seedCreate
	}
	result, err := g.dispatcher.Fetch(ctx, fetch.FetchRequest{Host: w.Name, Path: doc.path})
	if err != nil {
		g.log.Warn("memory seed policy check failed", "world", w.Name, "path", doc.path, "err", err)
		return seedFailed
	}
	meta := result.Response.Metadata
	switch result.Response.Status {
	case protocol.StatusNotFound:
		return seedCreate
	case protocol.StatusOK:
		if meta["version"] == "1" && meta["agent"] == publishpolicy.SeedAgent {
			return seedReplace
		}
		return seedKeep
	case protocol.StatusArchived:
		return seedKeep // archived counts as a deliberate act
	}
	g.log.Warn("memory seed policy check returned unexpected status", "world", w.Name, "path", doc.path, "status", result.Response.Status)
	return seedFailed
}

// publishMemorySeedDoc publishes one seed document and reports whether the
// world may still be marked seeded. replaceOnly is for a world that already
// holds the template: nothing is created there, only a server seed replaced.
func (g *mcpGateway) publishMemorySeedDoc(ctx context.Context, w *WorldConfig, doc *memorySeedDoc, replaceOnly bool) bool {
	action := g.memorySeedAction(ctx, w, doc)
	switch {
	case action == seedFailed:
		return false
	case action == seedKeep, replaceOnly && action != seedReplace:
		return true
	}
	body, readErr := memorySeedFS.ReadFile("memoryseed/" + doc.embedName)
	if readErr != nil {
		// Broken embed is a build defect; surface loudly but keep the tool call alive.
		g.log.Error("memory seed embed unreadable", "name", doc.embedName, "err", readErr)
		return false
	}
	// dispatchWithWriteAuth provisions the world write token and absorbs
	// first-mint Secret propagation lag, which a fresh world's first write hits.
	pres, pubErr := g.dispatchWithWriteAuth(ctx, w.Name, func(token string) (fetch.Result, error) {
		return g.dispatcher.Publish(ctx, fetch.WriteRequest{
			Host: w.Name, Path: doc.path, Body: string(body), Token: token,
			ExpectedVersion: action.expectedVersion(), Metadata: doc.meta,
		})
	})
	if pubErr != nil {
		g.log.Warn("memory seed publish failed", "world", w.Name, "path", doc.path, "err", pubErr)
		return false
	}
	switch pres.Response.Status {
	case protocol.StatusCreated, protocol.StatusOK, protocol.StatusConflict:
		return true // conflict = another replica or the user won the race; fine
	}
	g.log.Warn("memory seed publish returned unexpected status", "world", w.Name, "path", doc.path, "status", pres.Response.Status)
	return false
}
