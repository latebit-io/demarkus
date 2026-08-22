package filestore

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

func TestReadViewPinsFileAndCatalogState(t *testing.T) {
	raw := protocolstore.New(t.TempDir())
	store := New(raw, catalog.New())
	firstBody := []byte("# First\n")
	secondBody := []byte("# Second\n")
	if _, err := store.WriteVersion("/docs/doc.md", 0, firstBody, map[string]string{"tags": "first"}); err != nil {
		t.Fatalf("write v1: %v", err)
	}

	view, err := store.OpenReadView()
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := store.WriteVersion("/docs/doc.md", 1, secondBody, map[string]string{"tags": "second"})
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("write completed while view was open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	assertFileView(t, view, firstBody, "first")
	if err := view.Close(); err != nil {
		t.Fatalf("close view: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write v2: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write remained blocked after view close")
	}

	fresh, err := store.OpenReadView()
	if err != nil {
		t.Fatalf("open fresh view: %v", err)
	}
	t.Cleanup(func() {
		if err := fresh.Close(); err != nil {
			t.Errorf("close fresh view: %v", err)
		}
	})
	assertFileView(t, fresh, secondBody, "second")
}

func assertFileView(t *testing.T, view interface {
	Get(string, int) (*protocolstore.Document, error)
	LookupHashResult(string) (string, error)
	Lookup(string, catalog.Options) ([]catalog.Result, error)
}, body []byte, tag string) {
	t.Helper()
	document, err := view.Get("/docs/doc.md", 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(document.Content, body) {
		t.Errorf("body = %q, want %q", document.Content, body)
	}
	if path, err := view.LookupHashResult(protocolstore.ContentHash(body)); err != nil || path != "/docs/doc.md" {
		t.Errorf("hash lookup = %q, %v", path, err)
	}
	results, err := view.Lookup(tag, catalog.Options{})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(results) != 1 || results[0].Path != "/docs/doc.md" {
		t.Errorf("lookup results = %+v", results)
	}
	other := firstOrSecondBody(tag)
	if _, err := view.LookupHashResult(protocolstore.ContentHash(other)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("other hash error = %v, want not-found", err)
	}
}

func firstOrSecondBody(tag string) []byte {
	if tag == "first" {
		return []byte("# Second\n")
	}
	return []byte("# First\n")
}
