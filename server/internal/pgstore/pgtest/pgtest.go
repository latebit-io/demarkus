// Package pgtest opens pgstore against the shared test database for test
// packages. Each package gets its own schema so packages that go test runs
// in parallel cannot reset each other's tables.
package pgtest

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	// pgx database/sql driver for the schema bootstrap handle.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/pgstore"
)

// dsn returns DEMARKUS_TEST_PG_DSN or skips the test. With
// DEMARKUS_TEST_PG_REQUIRED set (CI) a missing DSN fails instead: backend
// parity must be proven on every run, never silently dropped.
func dsn(t testing.TB) string {
	t.Helper()
	v := os.Getenv("DEMARKUS_TEST_PG_DSN")
	if v == "" {
		if os.Getenv("DEMARKUS_TEST_PG_REQUIRED") != "" {
			t.Fatal("DEMARKUS_TEST_PG_DSN not set but DEMARKUS_TEST_PG_REQUIRED is; Postgres coverage is mandatory here")
		}
		t.Skip("DEMARKUS_TEST_PG_DSN not set; skipping Postgres test")
	}
	return v
}

// Lowercase only: CREATE SCHEMA quotes the name while search_path folds it,
// so mixed case would create one schema and search another.
var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// SchemaDSN returns the test DSN scoped to schema (created if missing) via
// search_path, for raw handles that must see the same tables as Open.
func SchemaDSN(t testing.TB, schema string) string {
	t.Helper()
	base := dsn(t)
	ensureSchema(t, base, schema)
	if strings.Contains(base, "search_path=") {
		t.Fatalf("DEMARKUS_TEST_PG_DSN must not set search_path; pgtest assigns one schema per package")
	}
	// libpq keyword/value DSNs take a space-separated parameter; URL DSNs a
	// query parameter.
	if !strings.HasPrefix(base, "postgres://") && !strings.HasPrefix(base, "postgresql://") {
		return base + " search_path=" + schema
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DEMARKUS_TEST_PG_DSN: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// RequireSerialFuzz fails a fuzz target started with more than one worker:
// workers are separate processes sharing the package schema and would reset
// each other's tables. Run with -parallel 1.
func RequireSerialFuzz(f *testing.F) {
	f.Helper()
	fuzz, parallel := flag.Lookup("test.fuzz"), flag.Lookup("test.parallel")
	if fuzz == nil || fuzz.Value.String() == "" || parallel == nil {
		return
	}
	if parallel.Value.String() != "1" {
		f.Fatalf("fuzzing against Postgres needs -parallel 1 (got %s): workers share one schema", parallel.Value.String())
	}
}

var (
	storesMu sync.Mutex
	stores   = map[string]*pgstore.Store{}
	raws     = map[string]*sql.DB{}
)

// Open returns the package's pgstore over schema, opened once per process and
// reset to empty on every call. Tests in a package run sequentially, so one
// shared pool is safe and saves a connect plus DDL per test.
func Open(t testing.TB, schema string) *pgstore.Store {
	t.Helper()
	storesMu.Lock()
	defer storesMu.Unlock()
	s, ok := stores[schema]
	if !ok {
		var err error
		s, err = pgstore.Open(SchemaDSN(t, schema), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("open postgres (schema %s): %v", schema, err)
		}
		stores[schema] = s
	}
	if err := s.Reset(context.Background()); err != nil {
		t.Fatalf("reset postgres (schema %s): %v", schema, err)
	}
	return s
}

// DB returns a raw handle on schema, opened once per process, for tests
// that must read or corrupt tables behind the store's back.
func DB(t testing.TB, schema string) *sql.DB {
	t.Helper()
	storesMu.Lock()
	defer storesMu.Unlock()
	if db, ok := raws[schema]; ok {
		return db
	}
	db, err := sql.Open("pgx", SchemaDSN(t, schema))
	if err != nil {
		t.Fatalf("open raw handle (schema %s): %v", schema, err)
	}
	raws[schema] = db
	return db
}

// Exec runs one statement on the schema's raw handle under a bounded context.
func Exec(t testing.TB, schema, query string, args ...any) sql.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := DB(t, schema).ExecContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	return res
}

// TamperVersion overwrites one version's stored bytes out of band, for
// tests that prove VerifyChain catches corruption. path is a request path.
func TamperVersion(t testing.TB, schema, path string, version int, stored []byte) {
	t.Helper()
	rel, err := store.RelPath(path)
	if err != nil {
		t.Fatalf("tamper %s: %v", path, err)
	}
	res := Exec(t, schema, `UPDATE versions SET stored = $3 WHERE path = $1 AND version = $2`, rel, version, stored)
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("tamper %s v%d: %d rows affected (err %v), want 1", path, version, n, err)
	}
}

// ensureSchema creates the schema. The name is validated here, next to the
// identifier interpolation, so every caller gets the guarantee.
func ensureSchema(t testing.TB, dsn, schema string) {
	t.Helper()
	if !schemaName.MatchString(schema) {
		t.Fatalf("pgtest schema %q: must match %s", schema, schemaName)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open bootstrap handle: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close bootstrap handle: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
}
