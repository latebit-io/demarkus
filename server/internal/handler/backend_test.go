package handler

import (
	"context"
	"os"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
)

// backend is one DocumentStore, wired as main.go does. Tamper overwrites one version's stored bytes behind the store's back so
// chain verification can be proven on every backend.
type backend struct {
	Store  DocumentStore
	Tamper func(t testing.TB, path string, version int, stored []byte)
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
	return backend{Store: filestore.New(s, catalog.New()), Tamper: tamper}
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
	if _, err := direct(b).WriteVersion(path, -1, body, meta); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newHandler wires a backend into a Handler as main.go does; ts may be nil
// for read-only tests.
func newHandler(b backend, ts *auth.TokenStore) *Handler {
	h := &Handler{Store: b.Store, Logger: discardLogger}
	if ts != nil {
		h.GetTokenStore = func() *auth.TokenStore { return ts }
	}
	return h
}

// viewStore wraps every view a store opens, so a test can replace one read.
type viewStore struct {
	DocumentStore
	wrap func(storagebackend.ReadView) storagebackend.ReadView
}

func (s *viewStore) OpenReadView(ctx context.Context) (storagebackend.ReadView, error) {
	view, err := s.DocumentStore.OpenReadView(ctx)
	if err != nil {
		return nil, err
	}
	return s.wrap(view), nil
}

// direct reaches under the handler to seed and inspect the store.
func direct(b backend) backendtest.Direct { return backendtest.Direct{Store: b.Store} }
