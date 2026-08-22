package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
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
		version, err := s.CurrentVersion("/../secret.md")
		if version != 0 || !errors.Is(err, os.ErrNotExist) {
			t.Errorf("CurrentVersion traversal = (%d, %v), want (0, ErrNotExist)", version, err)
		}
	})
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
	if _, _, err := s.Archive("/gone.md", true); err != nil {
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
	if _, _, err := s.Archive("/gone.md", false); err != nil {
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
