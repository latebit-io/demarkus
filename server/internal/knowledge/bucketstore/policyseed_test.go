package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

func defaultSeed() PolicySeed {
	return PolicySeed{
		Body:     []byte("# Write Policy\n\nDefault.\n\nstrictness: warn\n"),
		Metadata: map[string]string{"title": "Write Policy", "tags": "category:governance", "importance": "1"},
	}
}

func TestOpenSeedsMissingPolicy(t *testing.T) {
	objects := initializedMemory(t)
	seed := defaultSeed()
	store, err := Open(context.Background(), objects, Options{
		WorldID: testWorldID, RequirePolicy: true, PolicySeed: &seed,
	})
	if err != nil {
		t.Fatalf("Open with seed: %v", err)
	}
	document, err := store.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		t.Fatalf("get seeded policy: %v", err)
	}
	if !bytes.Equal(document.Content, seed.Body) || document.Version != 1 {
		t.Fatalf("seeded policy = version %d %q", document.Version, document.Content)
	}
	// Enforcement must be live on the store the seeding open returned.
	if !store.requirePolicy {
		t.Error("store opened without enforcing the policy it seeded")
	}
}

func TestOpenLeavesCuratedPolicyInPlace(t *testing.T) {
	objects := initializedMemory(t)
	seed := defaultSeed()
	if _, err := Open(context.Background(), objects, Options{
		WorldID: testWorldID, RequirePolicy: true, PolicySeed: &seed,
	}); err != nil {
		t.Fatalf("first open: %v", err)
	}

	// The agent replaces the seed with the real policy, then the world
	// restarts: a restart must not revert curated policy.
	curated := "# Write Policy\n\nCurated.\n\nstrictness: block\nrequire_tags: category\n"
	store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("reopen for overwrite: %v", err)
	}
	seedPolicy(t, store, curated, 1)

	restarted, err := Open(context.Background(), objects, Options{
		WorldID: testWorldID, RequirePolicy: true, PolicySeed: &seed,
	})
	if err != nil {
		t.Fatalf("restart with seed: %v", err)
	}
	document, err := restarted.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		t.Fatalf("get policy after restart: %v", err)
	}
	if string(document.Content) != curated || document.Version != 2 {
		t.Fatalf("policy after restart = version %d %q, want curated v2", document.Version, document.Content)
	}
}

func TestSeedPolicyRejectsInvalidSeed(t *testing.T) {
	objects := initializedMemory(t)
	store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seed := PolicySeed{Body: []byte("strictness: nonsense\n"), Metadata: map[string]string{"tags": "category:governance"}}
	if _, err := store.SeedPolicy(seed); err == nil {
		t.Fatal("SeedPolicy accepted an unenforceable policy")
	}
	if _, err := store.Get(publishpolicy.DocumentPath, 0); err == nil {
		t.Error("rejected seed was written anyway")
	}
}

func TestEnsureWorld(t *testing.T) {
	t.Run("creates genesis in an empty bucket", func(t *testing.T) {
		objects := newTestMemory(t)
		created, err := EnsureWorld(context.Background(), objects, testWorldID)
		if err != nil {
			t.Fatalf("EnsureWorld: %v", err)
		}
		if !created {
			t.Error("created = false for an empty bucket")
		}
		if _, err := Open(context.Background(), objects, Options{WorldID: testWorldID}); err != nil {
			t.Fatalf("open created world: %v", err)
		}
	})

	t.Run("leaves an existing world alone", func(t *testing.T) {
		objects := initializedMemory(t)
		before := getObject(t, objects, headObjectKey).Attributes.Generation
		created, err := EnsureWorld(context.Background(), objects, testWorldID)
		if err != nil {
			t.Fatalf("EnsureWorld: %v", err)
		}
		if created {
			t.Error("created = true for an initialized bucket")
		}
		if after := getObject(t, objects, headObjectKey).Attributes.Generation; after != before {
			t.Errorf("head generation changed from %d to %d", before, after)
		}
	})

	t.Run("refuses a non-empty bucket with no head", func(t *testing.T) {
		objects := newTestMemory(t)
		if _, err := objects.Create(context.Background(), "someone-elses-data.json", []byte("{}")); err != nil {
			t.Fatalf("seed foreign object: %v", err)
		}
		_, err := EnsureWorld(context.Background(), objects, testWorldID)
		if !errors.Is(err, blob.ErrPrecondition) {
			t.Fatalf("EnsureWorld error = %v, want ErrPrecondition", err)
		}
		if !strings.Contains(err.Error(), "no world head") {
			t.Errorf("error does not name the cause: %v", err)
		}
	})
}
