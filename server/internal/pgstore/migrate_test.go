package pgstore_test

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// TestPostgresMigrationRoundTrip runs the shared migration gate against
// Postgres, then checks the pg-only surfaces of the imported world: in-tx
// catalog rows, hash index, unarchive visibility, duplicate-refusal atomicity.
func TestPostgresMigrationRoundTrip(t *testing.T) {
	s := openStore(t)
	storetest.RunMigrationRoundTrip(t, func(_ *testing.T) storetest.MigrationBackend {
		return storetest.MigrationBackend{Migrator: s, Store: s}
	})

	// LOOKUP works straight off the import: catalog rows rode the import tx.
	assertLookupCount(t, s, "unicode", 1)
	if _, err := s.LookupHash(store.ContentHash([]byte("# Deep\n"))); err != nil {
		t.Errorf("LookupHash imported body: %v", err)
	}

	// The refused duplicate import (run inside the shared suite) was
	// transactional: no partial version rows behind it.
	if got, err := s.CurrentVersion("/a.md"); err != nil || got != 3 {
		t.Errorf("after refused import: current = %d (err %v), want 3", got, err)
	}
	if vs, err := s.Versions("/a.md"); err != nil || len(vs) != 3 {
		t.Errorf("after refused import: %d versions (err %v), want 3", len(vs), err)
	}

	// The archived document's catalog row was imported too: unarchive
	// restores it to LOOKUP without a write.
	if _, _, err := s.Archive("/arch.md", false); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	rs, err := s.Lookup("archived", catalog.Options{})
	if err != nil {
		t.Fatalf("lookup after unarchive: %v", err)
	}
	if len(rs) != 1 {
		t.Errorf("lookup after unarchive: %d results, want 1 (title match)", len(rs))
	}

	// A later Init must not disturb the imported catalog (backfill is
	// insert-if-absent).
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init after import: %v", err)
	}
	assertLookupCount(t, s, "unicode", 1)
}
