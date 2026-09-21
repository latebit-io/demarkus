package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// openSeeded opens the world and seeds it as the knowledge server does.
func openSeeded(t *testing.T, objects blob.Store, seed writepolicy.PolicySeed, wantCreated bool) *Store {
	t.Helper()
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := writepolicy.Seed(context.Background(), store, seed)
	if err != nil || created != wantCreated {
		t.Fatalf("Seed = (%v, %v), want created %v", created, err, wantCreated)
	}
	return store
}

func defaultSeed() writepolicy.PolicySeed {
	return writepolicy.PolicySeed{
		Body:     []byte("# Write Policy\n\nDefault.\n\nstrictness: warn\n"),
		Metadata: policyMetadata(),
	}
}

func TestSeedCreatesMissingPolicy(t *testing.T) {
	objects := initializedMemory(t)
	seed := defaultSeed()
	store := openSeeded(t, objects, seed, true)
	document, err := store.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		t.Fatalf("get seeded policy: %v", err)
	}
	if !bytes.Equal(document.Content, seed.Body) || document.Version != 1 {
		t.Fatalf("seeded policy = version %d %q", document.Version, document.Content)
	}
}

func TestSeedCountsArchivedPolicyAsPresent(t *testing.T) {
	objects := initializedMemory(t)
	store := openSeeded(t, objects, defaultSeed(), true)
	if _, err := store.SetArchived(context.Background(), backend.ArchiveRequest{Path: publishpolicy.DocumentPath, Archived: true}); err != nil {
		t.Fatalf("archive policy: %v", err)
	}
	openSeeded(t, objects, defaultSeed(), false)
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
		if _, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID}); err != nil {
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

func TestSeedRefusesCanceledContext(t *testing.T) {
	objects := initializedMemory(t)
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Cancellation can land after the index phase, the one window where a
	// seeding write would otherwise outlive the open that asked for it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := writepolicy.Seed(ctx, store, defaultSeed()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Seed error = %v, want context canceled", err)
	}
	if _, err := store.Get(publishpolicy.DocumentPath, 0); err == nil {
		t.Error("canceled open seeded a policy anyway")
	}
}

func TestReadOnlyStoreRefusesEveryWrite(t *testing.T) {
	objects := initializedMemory(t)
	writable, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := writable.WriteVersion("/kept.md", 0, []byte("# Kept\n"), nil); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, ReadOnly: true})
	if err != nil {
		t.Fatalf("Open read-only: %v", err)
	}
	ctx := context.Background()
	if _, err := store.Publish(ctx, backend.WriteRequest{Path: "/new.md", Content: []byte("# New\n")}); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("Publish = %v, want ErrReadOnly", err)
	}
	if _, err := store.Append(ctx, backend.WriteRequest{Path: "/kept.md", ExpectedVersion: 1, Content: []byte("more\n")}); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("Append = %v, want ErrReadOnly", err)
	}
	if _, err := store.SetArchived(ctx, backend.ArchiveRequest{Path: "/kept.md", Archived: true}); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("SetArchived = %v, want ErrReadOnly", err)
	}
	// A seed is a write like any other: refused, never a silent skip.
	if _, err := writepolicy.Seed(ctx, store, defaultSeed()); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("Seed = %v, want ErrReadOnly", err)
	}
	document, err := store.Get("/kept.md", 0)
	if err != nil || document.Version != 1 || document.Archived {
		t.Errorf("fixture after refused writes = %+v, %v", document, err)
	}
}
