package knowledgeserver

import (
	"bytes"
	"context"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/knowledge/knowledgeseed"
	"github.com/latebit-io/demarkus/server/knowledgeserver/worldsfragment"
)

// tenantFragment is a provisioner's fragment holding one tenant, alice,
// seeded as seed says.
func tenantFragment(t *testing.T, seed string) string {
	t.Helper()
	tenant := worldsfragment.World{
		Name: "alice", Authorities: []string{"alice.memory.svc.cluster.local"}, Gated: true, Seed: seed,
		Bucket: worldsfragment.Bucket{URL: "gs://memory-alice", WorldID: testWorldID},
		Policy: &worldsfragment.Policy{Governance: "open"},
	}
	data, err := worldsfragment.Render([]worldsfragment.World{tenant})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// livePolicy is the policy the live world holds.
func livePolicy(t *testing.T, h *worldsTestHarness, world string) *storefmt.Document {
	t.Helper()
	document, err := backendtest.Direct{Store: h.store(t, world)}.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		t.Fatalf("policy of %s: %v", world, err)
	}
	return document
}

// A new tenant world holds the memory policy from its first version: the
// server seeds it, and no broker write comes after.
func TestNewTenantWorldIsSeededWithTheMemoryPolicy(t *testing.T) {
	h := newWorldsHarness(t, tenantFragment(t, worldsfragment.SeedMemory))
	policy := livePolicy(t, h, "alice")
	want := knowledgeseed.MemoryPolicySeed()
	if policy.Version != 1 || !bytes.Equal(policy.Content, want.Body) {
		t.Fatalf("policy = version %d %q, want the memory policy at version 1", policy.Version, policy.Content)
	}
	if got := policy.Metadata["agent"]; got != knowledgeseed.MemorySeedAgent {
		t.Fatalf("policy agent = %q, want %q", got, knowledgeseed.MemorySeedAgent)
	}
}

// A tenant world opened by a build that seeded the knowledge default still
// holds it at version one: the first open that knows it for a memory
// replaces it, once.
func TestTenantWorldOnTheDefaultSeedIsUpgradedOnceOnOpen(t *testing.T) {
	h := newWorldsHarness(t, tenantFragment(t, ""))
	before := livePolicy(t, h, "alice")
	if before.Version != 1 || before.Metadata["agent"] != publishpolicy.SeedAgent {
		t.Fatalf("fixture policy = version %d agent %q, want the default seed", before.Version, before.Metadata["agent"])
	}

	h.writeFragment(t, tenantFragment(t, worldsfragment.SeedMemory))
	if err := h.manager.Reload(); err != nil {
		t.Fatal(err)
	}
	upgraded := livePolicy(t, h, "alice")
	if upgraded.Version != 2 || !bytes.Equal(upgraded.Content, knowledgeseed.MemoryPolicySeed().Body) {
		t.Fatalf("policy after the upgrade = version %d %q, want the memory policy at version 2", upgraded.Version, upgraded.Content)
	}

	// The next open finds nothing left to upgrade.
	again, err := knowledgeseed.UpgradeToMemory(context.Background(), h.store(t, "alice"))
	if err != nil || again {
		t.Fatalf("second upgrade = %v, %v; want none", again, err)
	}
	if policy := livePolicy(t, h, "alice"); policy.Version != 2 {
		t.Fatalf("policy after a second upgrade = version %d, want it left at 2", policy.Version)
	}
}

// A policy its owner wrote is theirs: neither a later version nor a first
// version without the server's marker is replaced.
func TestUpgradeLeavesAnOwnersPolicyAlone(t *testing.T) {
	owners := []byte("# Write Policy\n\nMine.\n\nstrictness: warn\n")
	h := newWorldsHarness(t, tenantFragment(t, ""))
	if _, err := (backendtest.Direct{Store: h.store(t, "alice")}).WriteVersion(publishpolicy.DocumentPath, 1, owners, map[string]string{"agent": "alice@example.com"}); err != nil {
		t.Fatalf("owner's edit: %v", err)
	}
	h.writeFragment(t, tenantFragment(t, worldsfragment.SeedMemory))
	if err := h.manager.Reload(); err != nil {
		t.Fatal(err)
	}
	if policy := livePolicy(t, h, "alice"); policy.Version != 2 || !bytes.Equal(policy.Content, owners) {
		t.Fatalf("policy = version %d %q, want the owner's edit left at version 2", policy.Version, policy.Content)
	}
}
