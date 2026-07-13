package pgstore_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	// Register the pgx database/sql driver for the raw handle the backfill
	// test uses to clear the catalog table out of band.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/pgstore"
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
	})
}

// testDSN returns the conformance database DSN or skips the test.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DEMARKUS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DEMARKUS_TEST_PG_DSN not set; skipping Postgres test")
	}
	return dsn
}

// openStore opens a pgstore against the test database with a quiet logger.
func openStore(t *testing.T) *pgstore.Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := pgstore.Open(testDSN(t), logger)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPostgresLookupConformance runs the LOOKUP conformance suite against
// pgstore's transactional catalog. The store is both halves of the backend
// pair: catalog rows are maintained by its write transactions, and the
// suite's handler-mirroring Put/Remove calls are the same no-ops the real
// handler makes.
func TestPostgresLookupConformance(t *testing.T) {
	s := openStore(t)
	storetest.RunLookupConformance(t, func(t *testing.T) storetest.LookupBackend {
		if err := s.Reset(context.Background()); err != nil {
			t.Fatalf("reset: %v", err)
		}
		return storetest.LookupBackend{Store: s, Catalog: s}
	})
}

// mockStream is the minimal handler.Stream for driving requests in tests.
type mockStream struct {
	io.Reader
	output bytes.Buffer
}

func (m *mockStream) Write(p []byte) (int, error) { return m.output.Write(p) }
func (m *mockStream) Close() error                { return nil }

// send runs one raw request through a handler and returns the parsed response.
func send(t *testing.T, h *handler.Handler, request string) protocol.Response {
	t.Helper()
	stream := &mockStream{Reader: strings.NewReader(request)}
	h.HandleStream(stream)
	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp
}

// replicaHandler wraps a store in a handler the way main.go wires postgres
// mode: the store is both DocumentStore and LookupCatalog, no catalog build.
func replicaHandler(t *testing.T, s *pgstore.Store, ts *auth.TokenStore) *handler.Handler {
	t.Helper()
	return &handler.Handler{
		Store:         s,
		Catalog:       s,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		GetTokenStore: func() *auth.TokenStore { return ts },
	}
}

// TestPostgresLookupMultiReplica is the multi-replica proof: two handler and
// catalog instances over one database, with no shared process state. A write
// handled by one replica must be visible to LOOKUP on the other immediately,
// and an archive handled by the second must hide the document from the first.
func TestPostgresLookupMultiReplica(t *testing.T) {
	storeA, storeB := openStore(t), openStore(t)
	if err := storeA.Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	const secret = "replica-secret"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {Paths: []string{"/**"}, Operations: []string{"publish"}},
	})
	replicaA, replicaB := replicaHandler(t, storeA, ts), replicaHandler(t, storeB, ts)

	pub := send(t, replicaA, "PUBLISH /notes/auth.md\n---\nauth: "+secret+"\ntags: middleware,go\n---\n# Auth Middleware\n")
	if pub.Status != protocol.StatusCreated {
		t.Fatalf("publish via A: status = %q, want created", pub.Status)
	}

	look := send(t, replicaB, "LOOKUP /\n---\nquery: middleware\n---\n")
	if look.Status != protocol.StatusOK || look.Metadata["matches"] != "1" {
		t.Fatalf("lookup via B: status %q matches %q, want ok/1", look.Status, look.Metadata["matches"])
	}
	if !strings.Contains(look.Body, "/notes/auth.md") || !strings.Contains(look.Body, "Auth Middleware") {
		t.Errorf("lookup via B missing the document A published:\n%s", look.Body)
	}

	arch := send(t, replicaB, "ARCHIVE /notes/auth.md\n---\nauth: "+secret+"\n---\n")
	if arch.Status != protocol.StatusOK {
		t.Fatalf("archive via B: status = %q, want ok", arch.Status)
	}
	look = send(t, replicaA, "LOOKUP /\n---\nquery: middleware\n---\n")
	if look.Metadata["matches"] != "0" {
		t.Errorf("lookup via A after archive via B: matches = %q, want 0", look.Metadata["matches"])
	}
}

// TestPostgresCatalogBackfill covers the additive migration: a database with
// documents but no catalog rows (a world written before the catalog table
// existed) gets its catalog rebuilt from version tips on Init. Archived
// documents get rows too, staying hidden until unarchived. Init also
// reconciles partial gaps (documents that missed their catalog row, e.g.
// written by an old binary during a rolling deploy) without touching rows
// that already exist.
func TestPostgresCatalogBackfill(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.Reset(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := s.WriteVersion("/live.md", 0, []byte("# Live\n"), map[string]string{"tags": "go"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.WriteVersion("/gone.md", 0, []byte("# Gone\n"), map[string]string{"tags": "go"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Archive("/gone.md", true); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Simulate the pre-catalog world: rows exist for documents and versions
	// but the catalog table is empty.
	db, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `DELETE FROM catalog`); err != nil {
		t.Fatalf("clear catalog: %v", err)
	}
	assertLookupCount(t, s, "go", 0)

	if err := s.Init(ctx); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	assertLookupCount(t, s, "go", 1)

	// The archived document's row was backfilled too: unarchive restores it
	// without any write.
	if err := s.Archive("/gone.md", false); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	assertLookupCount(t, s, "go", 2)

	// A second Init on a populated catalog is a no-op, not a duplicate fill.
	if err := s.Init(ctx); err != nil {
		t.Fatalf("second init: %v", err)
	}
	assertLookupCount(t, s, "go", 2)

	// A partial gap (one document missing its row) is reconciled too.
	if _, err := db.ExecContext(ctx, `DELETE FROM catalog WHERE path = 'live.md'`); err != nil {
		t.Fatalf("clear one row: %v", err)
	}
	assertLookupCount(t, s, "go", 1)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("reconcile init: %v", err)
	}
	assertLookupCount(t, s, "go", 2)
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
