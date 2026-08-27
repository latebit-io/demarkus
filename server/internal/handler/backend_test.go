package handler

import (
	"os"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
)

// backend is one DocumentStore plus its LookupCatalog, wired as main.go does.
// Tamper overwrites one version's stored bytes behind the store's back so
// chain verification can be proven on every backend.
type backend struct {
	Store   DocumentStore
	Catalog LookupCatalog
	Views   storagebackend.ViewProvider
	Tamper  func(t testing.TB, path string, version int, stored []byte)
}

// backendFactory returns a fresh, empty backend.
type backendFactory func(t testing.TB) backend

// fileBackend is the file store over a temp root with the in-memory catalog.
func fileBackend(t testing.TB) backend { return fileBackendAt(t.TempDir()) }

// fileBackendAt roots the file store at dir, for tests that touch the disk.
func fileBackendAt(dir string) backend {
	s := store.New(dir)
	tamper := func(t testing.TB, path string, version int, stored []byte) {
		t.Helper()
		file, err := s.VersionFilePath(path, version)
		if err != nil {
			t.Fatalf("tamper %s v%d: %v", path, version, err)
		}
		if err := os.WriteFile(file, stored, 0o644); err != nil {
			t.Fatalf("tamper %s v%d: %v", path, version, err)
		}
	}
	wrapped := filestore.New(s, catalog.New())
	return backend{Store: wrapped, Catalog: wrapped, Views: wrapped, Tamper: tamper}
}

// forEachBackend runs fn per backend; only the file store remains, and the
// fan-out is kept as the seam for a future backend (ADR 0010).
func forEachBackend(t *testing.T, fn func(t *testing.T, newBackend backendFactory)) {
	t.Run("file", func(t *testing.T) { fn(t, fileBackend) })
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
	h := &Handler{Store: b.Store, Catalog: b.Catalog, Views: b.Views, Logger: discardLogger}
	if ts != nil {
		h.GetTokenStore = func() *auth.TokenStore { return ts }
	}
	return h
}
