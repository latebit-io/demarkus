package handler

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/pgstore/pgtest"
)

// backend is one DocumentStore plus its LookupCatalog, wired as main.go does.
type backend struct {
	Store   DocumentStore
	Catalog LookupCatalog
}

// backendFactory returns a fresh, empty backend.
type backendFactory func(t testing.TB) backend

// fileBackend is the file store over a temp root with the in-memory catalog.
func fileBackend(t testing.TB) backend { return fileBackendAt(t.TempDir()) }

// fileBackendAt roots the file store at dir, for tests that touch the disk.
func fileBackendAt(dir string) backend {
	return backend{Store: store.New(dir), Catalog: catalog.New()}
}

// postgresBackend returns the package's shared pgstore (own schema, reset)
// as both halves: catalog rows ride its write transactions. Skips without
// DEMARKUS_TEST_PG_DSN unless DEMARKUS_TEST_PG_REQUIRED is set.
func postgresBackend(t testing.TB) backend {
	t.Helper()
	s := pgtest.Open(t, "handler")
	return backend{Store: s, Catalog: s}
}

// forEachBackend runs fn once per backend so every handler test proves the
// same protocol behavior over both stores.
func forEachBackend(t *testing.T, fn func(t *testing.T, newBackend backendFactory)) {
	t.Run("file", func(t *testing.T) { fn(t, fileBackend) })
	t.Run("postgres", func(t *testing.T) { fn(t, postgresBackend) })
}

// seedBackend writes files (name → body, no leading slash) as version 1.
func seedBackend(t testing.TB, b backend, files map[string]string) {
	t.Helper()
	for name, content := range files {
		mustWrite(t, b, "/"+name, []byte(content), nil)
	}
}

// mustWrite writes the next version of path through the store.
func mustWrite(t testing.TB, b backend, path string, body []byte, meta map[string]string) {
	t.Helper()
	if _, err := b.Store.WriteVersion(path, -1, body, meta); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newHandler wires a backend into a Handler as main.go does; ts may be nil
// for read-only tests.
func newHandler(b backend, ts *auth.TokenStore) *Handler {
	h := &Handler{Store: b.Store, Catalog: b.Catalog, Logger: discardLogger}
	if ts != nil {
		h.GetTokenStore = func() *auth.TokenStore { return ts }
	}
	return h
}
