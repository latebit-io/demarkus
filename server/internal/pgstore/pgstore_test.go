package pgstore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/pgstore"
	"github.com/latebit-io/demarkus/server/internal/pgstore/pgtest"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// TestPostgresConformance runs the shared DocumentStore conformance suite
// against Postgres. Gated on DEMARKUS_TEST_PG_DSN so environments without a
// database skip instead of fail, e.g.:
//
//	DEMARKUS_TEST_PG_DSN="postgres://postgres:postgres@localhost:5432/demarkus_test" go test ./internal/pgstore/
func TestPostgresConformance(t *testing.T) {
	s := openStore(t)
	storetest.RunConformance(t, func(t *testing.T) handler.DocumentStore {
		if err := s.Reset(context.Background()); err != nil {
			t.Fatalf("reset: %v", err)
		}
		return s
	}, pgTamper)
}

// pgTamper is the Postgres Tamper: a raw UPDATE on the package schema.
func pgTamper(t testing.TB, _ handler.DocumentStore, path string, version int, stored []byte) {
	pgtest.TamperVersion(t, pgSchema, path, version, stored)
}

// pgSchema isolates this package's tables from other test packages.
const pgSchema = "pgstore"

// openStore returns the package's shared pgstore, reset to empty.
func openStore(t testing.TB) *pgstore.Store {
	t.Helper()
	return pgtest.Open(t, pgSchema)
}

func TestPostgresCurrentVersionRejectsTraversal(t *testing.T) {
	t.Run("returns RelPath error", func(t *testing.T) {
		s := pgstore.NewWithDB(newReadViewDriverDB(t, &readViewDriverState{}), nil)
		version, err := s.CurrentVersionResult("/../secret.md")
		if version != 0 || !errors.Is(err, os.ErrNotExist) {
			t.Errorf("CurrentVersion traversal = (%d, %v), want (0, ErrNotExist)", version, err)
		}
	})
}

func TestPostgresArchiveStateIsVersionImmutable(t *testing.T) {
	s := openStore(t)
	created, err := s.WriteVersion("/immutable.md", 0, []byte("v1"), map[string]string{"tags": "archive"})
	if err != nil {
		t.Fatalf("write v1: %v", err)
	}

	type versionRow struct {
		stored   []byte
		modified time.Time
	}
	readRow := func(version int) versionRow {
		t.Helper()
		var row versionRow
		err := pgtest.DB(t, pgSchema).QueryRow(
			`SELECT stored, modified FROM versions WHERE path = 'immutable.md' AND version = $1`, version,
		).Scan(&row.stored, &row.modified)
		if err != nil {
			t.Fatalf("read v%d row: %v", version, err)
		}
		return row
	}
	assertIdentity := func(label string, got *store.Document, want versionRow) {
		t.Helper()
		if got.ETag != store.StoredETag(want.stored) || !got.Modified.Equal(want.modified.UTC().Truncate(time.Second)) {
			t.Errorf("%s identity = (etag %q, modified %v), want (%q, %v)",
				label, got.ETag, got.Modified, store.StoredETag(want.stored), want.modified.UTC().Truncate(time.Second))
		}
	}
	initial := readRow(1)
	assertIdentity("created", created, initial)

	archived, changed, err := s.ArchiveResult("/immutable.md", true)
	if err != nil || !changed || !archived.Archived {
		t.Fatalf("archive = (%+v, %v, %v), want archived transition", archived, changed, err)
	}
	assertIdentity("archive", archived, initial)
	afterArchive := readRow(1)
	if !bytes.Equal(afterArchive.stored, initial.stored) || !afterArchive.modified.Equal(initial.modified) {
		t.Errorf("archive changed version row: stored equal=%v, modified %v -> %v",
			bytes.Equal(afterArchive.stored, initial.stored), initial.modified, afterArchive.modified)
	}
	current, err := s.Get("/immutable.md", 0)
	if err != nil || !current.Archived {
		t.Fatalf("current after archive = (%+v, %v), want archived", current, err)
	}
	pinned, err := s.Get("/immutable.md", 1)
	if err != nil || pinned.Archived {
		t.Fatalf("pinned after archive = (%+v, %v), want unarchived history", pinned, err)
	}
	assertIdentity("pinned after archive", pinned, initial)

	unarchived, changed, err := s.ArchiveResult("/immutable.md", false)
	if err != nil || !changed || unarchived.Archived {
		t.Fatalf("unarchive = (%+v, %v, %v), want unarchived transition", unarchived, changed, err)
	}
	assertIdentity("unarchive", unarchived, initial)
	afterUnarchive := readRow(1)
	if !bytes.Equal(afterUnarchive.stored, initial.stored) || !afterUnarchive.modified.Equal(initial.modified) {
		t.Errorf("unarchive changed version row: stored equal=%v, modified %v -> %v",
			bytes.Equal(afterUnarchive.stored, initial.stored), initial.modified, afterUnarchive.modified)
	}

	legacyArchived, err := store.SetArchived(initial.stored, true)
	if err != nil {
		t.Fatalf("make legacy archived bytes: %v", err)
	}
	pgtest.TamperVersion(t, pgSchema, "/immutable.md", 1, legacyArchived)
	current, err = s.Get("/immutable.md", 0)
	if err != nil || current.Archived || current.ETag != store.StoredETag(legacyArchived) {
		t.Fatalf("current with contradictory header = (%+v, %v), want live row state and stored ETag", current, err)
	}
	if _, err := s.WriteVersion("/immutable.md", 1, []byte("v2"), nil); err != nil {
		t.Fatalf("write after legacy archived header: %v", err)
	}
	if err := s.VerifyChain("/immutable.md"); err != nil {
		t.Errorf("verify chain with historical archived bytes: %v", err)
	}

	tip := readRow(2)
	contradictoryTip, err := store.SetArchived(tip.stored, true)
	if err != nil {
		t.Fatalf("make contradictory tip: %v", err)
	}
	pgtest.TamperVersion(t, pgSchema, "/immutable.md", 2, contradictoryTip)
	current, err = s.Get("/immutable.md", 0)
	if err != nil || current.Archived {
		t.Fatalf("current with contradictory tip = (%+v, %v), want live row state", current, err)
	}
	archived, changed, err = s.ArchiveResult("/immutable.md", true)
	if err != nil || !changed || !archived.Archived {
		t.Fatalf("archive contradictory tip = (%+v, %v, %v), want row transition", archived, changed, err)
	}
	contradictoryRow := readRow(2)
	if !bytes.Equal(contradictoryRow.stored, contradictoryTip) || !contradictoryRow.modified.Equal(tip.modified) {
		t.Error("archive rewrote contradictory tip row")
	}
	assertIdentity("archive contradictory tip", archived, contradictoryRow)
	pinned, err = s.Get("/immutable.md", 2)
	if err != nil || pinned.Archived {
		t.Fatalf("pinned contradictory tip = (%+v, %v), want Archived false", pinned, err)
	}
}

// TestPostgresLookupConformance runs the LOOKUP conformance suite; the store
// is both halves of the backend pair (rows maintained by its write txs).
func TestPostgresLookupConformance(t *testing.T) {
	s := openStore(t)
	storetest.RunLookupConformance(t, func(t *testing.T) storetest.LookupBackend { return pgBackend(t, s) })
}

// pgBackend resets the shared store and returns it as both halves of the
// lookup pair (catalog rows are maintained by its write transactions).
func pgBackend(t testing.TB, s *pgstore.Store) storetest.LookupBackend {
	t.Helper()
	if err := s.Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	return storetest.LookupBackend{Store: s, Catalog: s, Views: s}
}

// TestPostgresLookupMultiReplica: two handler+catalog instances over one
// database, no shared process state. A write via one must be visible to
// LOOKUP on the other immediately; an archive via B must hide it from A.
func TestPostgresLookupMultiReplica(t *testing.T) {
	// Two pools over one schema, not the shared cached store: the point is
	// zero shared process state between the replicas.
	storeA := openStore(t)
	storeB, err := pgstore.Open(pgtest.SchemaDSN(t, pgSchema), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open replica B: %v", err)
	}
	t.Cleanup(func() {
		if err := storeB.Close(); err != nil {
			t.Errorf("close replica B: %v", err)
		}
	})
	secret := storetest.HandlerToken
	replicaA := storetest.NewHandler(storetest.LookupBackend{Store: storeA, Catalog: storeA, Views: storeA})
	replicaB := storetest.NewHandler(storetest.LookupBackend{Store: storeB, Catalog: storeB, Views: storeB})

	pub := storetest.SendRaw(t, replicaA, "PUBLISH /notes/auth.md\n---\nauth: "+secret+"\ntags: middleware,go\n---\n# Auth Middleware\n")
	if pub.Status != protocol.StatusCreated {
		t.Fatalf("publish via A: status = %q, want created", pub.Status)
	}

	look := storetest.SendRaw(t, replicaB, "LOOKUP /\n---\nquery: middleware\n---\n")
	if look.Status != protocol.StatusOK || look.Metadata["matches"] != "1" {
		t.Fatalf("lookup via B: status %q matches %q, want ok/1", look.Status, look.Metadata["matches"])
	}
	if !strings.Contains(look.Body, "/notes/auth.md") || !strings.Contains(look.Body, "Auth Middleware") {
		t.Errorf("lookup via B missing the document A published:\n%s", look.Body)
	}

	arch := storetest.SendRaw(t, replicaB, "ARCHIVE /notes/auth.md\n---\nauth: "+secret+"\n---\n")
	if arch.Status != protocol.StatusOK {
		t.Fatalf("archive via B: status = %q, want ok", arch.Status)
	}
	look = storetest.SendRaw(t, replicaA, "LOOKUP /\n---\nquery: middleware\n---\n")
	if look.Metadata["matches"] != "0" {
		t.Errorf("lookup via A after archive via B: matches = %q, want 0", look.Metadata["matches"])
	}
}

// TestPostgresCatalogBackfill covers the additive migration: Init rebuilds
// missing catalog rows from version tips (full pre-catalog worlds and
// partial rolling-deploy gaps), including archived docs, without touching
// existing rows.
func TestPostgresCatalogBackfill(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.WriteVersion("/live.md", 0, []byte("# Live\n"), map[string]string{"tags": "go"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.WriteVersion("/gone.md", 0, []byte("# Gone\n"), map[string]string{"tags": "go"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.ArchiveResult("/gone.md", true); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Simulate the pre-catalog world: rows exist for documents and versions
	// but the catalog table is empty.
	pgtest.Exec(t, pgSchema, `DELETE FROM catalog`)
	assertLookupCount(t, s, "go", 0)

	if err := s.Init(ctx); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	assertLookupCount(t, s, "go", 1)

	// The archived document's row was backfilled too: unarchive restores it
	// without any write.
	if _, _, err := s.ArchiveResult("/gone.md", false); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	assertLookupCount(t, s, "go", 2)

	// A second Init on a populated catalog is a no-op, not a duplicate fill.
	if err := s.Init(ctx); err != nil {
		t.Fatalf("second init: %v", err)
	}
	assertLookupCount(t, s, "go", 2)

	// A partial gap (one document missing its row) is reconciled too.
	pgtest.Exec(t, pgSchema, `DELETE FROM catalog WHERE path = 'live.md'`)
	assertLookupCount(t, s, "go", 1)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("reconcile init: %v", err)
	}
	assertLookupCount(t, s, "go", 2)
}

// TestPostgresCatalogBackfillBatches forces the backfill past one batch
// (batch size 100), proving the loop and its per-batch commits terminate.
func TestPostgresCatalogBackfillBatches(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	const docs = 120
	for i := range docs {
		path := fmt.Sprintf("/batch/doc-%03d.md", i)
		if _, err := s.WriteVersion(path, 0, []byte("x"), map[string]string{"tags": "batch"}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	pgtest.Exec(t, pgSchema, `DELETE FROM catalog`)
	assertLookupCount(t, s, "batch", 0)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	assertLookupCount(t, s, "batch", docs)
}

// assertLookupCount asserts how many documents a lookup matches.
func assertLookupCount(t *testing.T, s *pgstore.Store, query string, want int) {
	t.Helper()
	rs, err := s.Lookup(query, catalog.Options{})
	if err != nil {
		t.Fatalf("lookup %q: %v", query, err)
	}
	if len(rs) != want {
		t.Errorf("lookup %q matched %d, want %d (results %+v)", query, len(rs), want, rs)
	}
}

// TestPostgresDifferential is the primary parity gate: the file store is the
// reference implementation and pgstore must be observably indistinguishable
// from it over seeded random operation sequences.
func TestPostgresDifferential(t *testing.T) {
	s := openStore(t)
	ref := func(t *testing.T) storetest.LookupBackend { return storetest.FileBackend(t) }
	cand := func(t *testing.T) storetest.LookupBackend { return pgBackend(t, s) }
	storetest.RunDifferential(t, ref, cand, storetest.DifferentialConfig{})
}

// FuzzPostgresDifferential lets the fuzzer pick seeds for deeper runs. Fuzz
// workers share one database, so: go test -run '^$' -fuzz FuzzPostgresDifferential -fuzztime 2m -parallel 1
func FuzzPostgresDifferential(f *testing.F) {
	pgtest.RequireSerialFuzz(f)
	s := openStore(f)
	f.Add(int64(99))
	f.Fuzz(func(t *testing.T, seed int64) {
		storetest.RunDifferentialSeed(t, storetest.FileBackend(t), pgBackend(t, s), seed, storetest.DifferentialConfig{Ops: 100})
	})
}

// TestPostgresHandlerDifferential is the handler-level parity gate: the same
// request sequence through a file-backed and a pg-backed Handler.
func TestPostgresHandlerDifferential(t *testing.T) {
	s := openStore(t)
	ref := func(t *testing.T) storetest.LookupBackend { return storetest.FileBackend(t) }
	cand := func(t *testing.T) storetest.LookupBackend { return pgBackend(t, s) }
	storetest.RunHandlerDifferential(t, ref, cand, storetest.DifferentialConfig{})
}

// FuzzPostgresHandlerDifferential: deeper handler-level runs; -parallel 1.
func FuzzPostgresHandlerDifferential(f *testing.F) {
	pgtest.RequireSerialFuzz(f)
	s := openStore(f)
	f.Add(int64(7))
	f.Fuzz(func(t *testing.T, seed int64) {
		storetest.RunHandlerDifferentialSeed(t, storetest.FileBackend(t), pgBackend(t, s), seed, storetest.DifferentialConfig{Ops: 100})
	})
}

func BenchmarkPostgresHandler(b *testing.B) {
	storetest.RunHandlerBenchmarks(b, func(t testing.TB) storetest.LookupBackend { return pgBackend(t, openStore(t)) })
}
