package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// The knowledge gateway's dispatcher never reaches a memory tenant, even
// though the shared dispatcher behind it serves every world in the process.
func TestScopedDispatcherRefusesWorldsOutsideTheProfileView(t *testing.T) {
	cfg := brokertest.NewMemoryConfig()
	cfg.Worlds = append(cfg.Worlds, brokertest.NewConfig().Worlds...)
	inner := seededDispatcher()
	inner.Published["team-a/index.md"] = inner.Published["alice-w/index.md"]
	knowledge := scopeDispatcher(cfg.Registry().View(core.ProfileKnowledge), inner)

	var notFound *errWorldNotFound
	if _, err := knowledge.Fetch(context.Background(), fetch.FetchRequest{Host: "alice-w", Path: "/index.md"}); !errors.As(err, &notFound) {
		t.Fatalf("memory world through the knowledge view err = %v, want errWorldNotFound", err)
	}
	if len(inner.FetchCalls) != 0 {
		t.Fatal("a refused fetch reached the shared dispatcher")
	}
	if _, err := knowledge.Fetch(context.Background(), fetch.FetchRequest{Host: "team-a", Path: "/index.md"}); err != nil {
		t.Fatalf("knowledge world: %v", err)
	}
	memory := scopeDispatcher(cfg.Registry().View(core.ProfileMemory), inner)
	if _, err := memory.Lookup(context.Background(), fetch.LookupRequest{Host: "team-a"}); !errors.As(err, &notFound) {
		t.Fatalf("knowledge world through the memory view err = %v, want errWorldNotFound", err)
	}
	if got := scopeDispatcher(nil, inner); got != WorldDispatcher(inner) {
		t.Fatal("a nil view must leave the dispatcher unscoped")
	}
}
