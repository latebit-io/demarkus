package pgstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

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
	if _, err := s.LookupHashResult(store.ContentHash([]byte("# Deep\n"))); err != nil {
		t.Errorf("LookupHash imported body: %v", err)
	}

	// The refused duplicate import (run inside the shared suite) was
	// transactional: no partial version rows behind it.
	if got, err := s.CurrentVersionResult("/a.md"); err != nil || got != 3 {
		t.Errorf("after refused import: current = %d (err %v), want 3", got, err)
	}
	if vs, err := s.Versions("/a.md"); err != nil || len(vs) != 3 {
		t.Errorf("after refused import: %d versions (err %v), want 3", len(vs), err)
	}

	// The archived document's catalog row was imported too: unarchive
	// restores it to LOOKUP without a write.
	if _, _, err := s.ArchiveResult("/arch.md", false); err != nil {
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

func TestPostgresMigrationUsesExplicitArchiveState(t *testing.T) {
	s := openStore(t)
	stored, err := store.SerializeVersion(1, nil, []byte("# Imported\n"), map[string]string{"tags": "imported"})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	modified := time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC)
	document := store.StoredDocument{
		Versions: []store.StoredVersion{{Version: 1, Stored: stored, Modified: modified}},
		Archived: true,
	}
	if err := s.ImportDoc(context.Background(), "/explicit.md", document); err != nil {
		t.Fatalf("import: %v", err)
	}

	current, err := s.Get("/explicit.md", 0)
	if err != nil || !current.Archived {
		t.Fatalf("current = (%+v, %v), want explicitly archived", current, err)
	}
	pinned, err := s.Get("/explicit.md", 1)
	if err != nil || pinned.Archived {
		t.Fatalf("pinned = (%+v, %v), want Archived false", pinned, err)
	}
	if _, err := s.WriteVersion("/explicit.md", 1, []byte("changed"), nil); !errors.Is(err, store.ErrArchived) {
		t.Errorf("write with archived row and live stored header: %v, want ErrArchived", err)
	}

	var exported store.StoredDocument
	if err := s.ExportDocs(context.Background(), func(path string, got store.StoredDocument) error {
		if path != "/explicit.md" {
			t.Errorf("export path = %q, want /explicit.md", path)
		}
		exported = got
		return nil
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !exported.Archived || len(exported.Versions) != 1 {
		t.Fatalf("export = %+v, want one explicitly archived version", exported)
	}
	got := exported.Versions[0]
	if got.Version != 1 || !bytes.Equal(got.Stored, stored) || !got.Modified.Equal(modified) {
		t.Errorf("exported version = %+v, want byte-identical v1 modified %v", got, modified)
	}
}
