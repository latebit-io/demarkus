// Package storetest is the DocumentStore conformance suite. Every backend
// (the file store, pgstore, any future one) must pass it unchanged:
// behavior divergence between backends is a protocol bug, not a backend
// detail. The file store runs it unconditionally; database-backed stores
// run it gated on their test DSN.
package storetest

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// Factory returns a fresh, empty DocumentStore for one conformance subtest.
type Factory func(t *testing.T) handler.DocumentStore

// RunConformance exercises the DocumentStore contract against the backend
// the factory produces.
func RunConformance(t *testing.T, factory Factory) {
	subtests := []struct {
		name string
		fn   func(t *testing.T, s handler.DocumentStore)
	}{
		{"WriteGetRoundtrip", testWriteGetRoundtrip},
		{"DocumentOwnedFrontmatter", testDocumentOwnedFrontmatter},
		{"VersionHistory", testVersionHistory},
		{"ConflictSemantics", testConflictSemantics},
		{"NotModifiedDedup", testNotModifiedDedup},
		{"ArchiveLifecycle", testArchiveLifecycle},
		{"AppendSemantics", testAppendSemantics},
		{"ListDirSemantics", testListDirSemantics},
		{"IsDirSemantics", testIsDirSemantics},
		{"RetentionPrune", testRetentionPrune},
		{"MissingDocument", testMissingDocument},
		{"PathCollisions", testPathCollisions},
		{"RejectsBinaryBody", testRejectsBinaryBody},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			st.fn(t, factory(t))
		})
	}
}

func testWriteGetRoundtrip(t *testing.T, s handler.DocumentStore) {
	meta := map[string]string{"tags": "a, b", "importance": "0.5"}
	doc, err := s.WriteVersion("/notes/first.md", 0, []byte("# First\n"), meta)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("create version = %d, want 1", doc.Version)
	}
	// Contract: Content is body-only on write returns too.
	if !bytes.Equal(doc.Content, []byte("# First\n")) {
		t.Errorf("write-returned body = %q, want %q", doc.Content, "# First\n")
	}
	got, err := s.Get("/notes/first.md", 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Contract: Content is body-only on every path — no store frontmatter.
	if !bytes.Equal(got.Content, []byte("# First\n")) {
		t.Errorf("body = %q, want %q", got.Content, "# First\n")
	}
	if got.Version != 1 || got.Archived {
		t.Errorf("got version=%d archived=%v, want 1 false", got.Version, got.Archived)
	}
	if got.Metadata["tags"] == "" || got.Metadata["importance"] != "0.5" {
		t.Errorf("metadata roundtrip = %v", got.Metadata)
	}
}

// testDocumentOwnedFrontmatter pins that a body beginning with its own YAML
// fence survives byte-exact: only the store's frontmatter may be stripped,
// never the document's (a second ExtractBody would eat it).
func testDocumentOwnedFrontmatter(t *testing.T, s handler.DocumentStore) {
	body := []byte("---\ntitle: mine\n---\n# Doc\n")
	doc, err := s.WriteVersion("/fm.md", 0, body, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !bytes.Equal(doc.Content, body) {
		t.Errorf("write-returned body = %q, want %q", doc.Content, body)
	}
	got, err := s.Get("/fm.md", 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got.Content, body) {
		t.Errorf("get body = %q, want %q", got.Content, body)
	}
}

func testVersionHistory(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/doc.md", 0, "v1")
	mustWrite(t, s, "/doc.md", 1, "v2")
	mustWrite(t, s, "/doc.md", 2, "v3")

	if cur := s.CurrentVersion("/doc.md"); cur != 3 {
		t.Errorf("CurrentVersion = %d, want 3", cur)
	}
	old, err := s.Get("/doc.md", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if string(old.Content) != "v1" {
		t.Errorf("v1 body = %q", old.Content)
	}
	vs, err := s.Versions("/doc.md")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vs) != 3 || vs[0].Version != 3 || vs[2].Version != 1 {
		t.Errorf("versions = %+v, want 3 newest-first", vs)
	}
	if err := s.VerifyChain("/doc.md"); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func testConflictSemantics(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/c.md", 0, "v1")

	// Create-only on an existing document.
	doc, err := s.WriteVersion("/c.md", 0, []byte("x"), nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create-only on existing: err = %v, want ErrConflict", err)
	}
	if doc == nil || doc.Version != 1 {
		t.Errorf("conflict doc = %+v, want Version 1", doc)
	}

	// Stale expected version.
	mustWrite(t, s, "/c.md", 1, "v2")
	doc, err = s.WriteVersion("/c.md", 1, []byte("x"), nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale expected: err = %v, want ErrConflict", err)
	}
	if doc == nil || doc.Version != 2 {
		t.Errorf("conflict doc = %+v, want Version 2", doc)
	}

	// Skip-check write (< 0) always lands on top.
	doc, err = s.WriteVersion("/c.md", -1, []byte("v3"), nil)
	if err != nil || doc.Version != 3 {
		t.Errorf("unchecked write = (%+v, %v), want v3", doc, err)
	}
}

func testNotModifiedDedup(t *testing.T, s handler.DocumentStore) {
	meta := map[string]string{"tags": "same"}
	if _, err := s.WriteVersion("/dup.md", 0, []byte("body"), meta); err != nil {
		t.Fatalf("create: %v", err)
	}
	doc, err := s.WriteVersion("/dup.md", 1, []byte("body"), meta)
	if !errors.Is(err, store.ErrNotModified) {
		t.Fatalf("identical rewrite: err = %v, want ErrNotModified", err)
	}
	if doc == nil || doc.Version != 1 {
		t.Errorf("not-modified doc = %+v, want Version 1", doc)
	}
	if cur := s.CurrentVersion("/dup.md"); cur != 1 {
		t.Errorf("CurrentVersion after dedup = %d, want 1", cur)
	}
	// Same body, different metadata is a real new version.
	doc, err = s.WriteVersion("/dup.md", 1, []byte("body"), map[string]string{"tags": "changed"})
	if err != nil || doc.Version != 2 {
		t.Errorf("meta change = (%+v, %v), want v2", doc, err)
	}
}

func testArchiveLifecycle(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/arch.md", 0, "content")
	hash := store.ContentHash([]byte("content"))

	if p, ok := s.LookupHash(hash); !ok || p != "/arch.md" {
		t.Errorf("LookupHash before archive = (%q, %v), want /arch.md", p, ok)
	}
	if err := s.Archive("/arch.md", true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got, err := s.Get("/arch.md", 0)
	if err != nil {
		t.Fatalf("get archived: %v", err)
	}
	if !got.Archived {
		t.Error("archived flag not set on Get")
	}
	if _, ok := s.LookupHash(hash); ok {
		t.Error("archived doc still resolvable by hash")
	}
	if _, err := s.WriteVersion("/arch.md", 1, []byte("new"), nil); !errors.Is(err, store.ErrArchived) {
		t.Errorf("write to archived: err = %v, want ErrArchived", err)
	}
	if err := s.Archive("/arch.md", false); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if p, ok := s.LookupHash(hash); !ok || p != "/arch.md" {
		t.Errorf("LookupHash after unarchive = (%q, %v), want /arch.md", p, ok)
	}
	if doc, err := s.WriteVersion("/arch.md", 1, []byte("new"), nil); err != nil || doc.Version != 2 {
		t.Errorf("write after unarchive = (%+v, %v), want v2", doc, err)
	}
	if err := s.Archive("/missing.md", true); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("archive missing: err = %v, want ErrNotExist", err)
	}
}

func testAppendSemantics(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/log.md", 0, "line1")

	doc, err := s.Append("/log.md", 1, []byte("line2"), nil)
	if err != nil || doc.Version != 2 {
		t.Fatalf("append = (%+v, %v), want v2", doc, err)
	}
	// Contract: Content is body-only on append returns too.
	if string(doc.Content) != "line1\nline2" {
		t.Errorf("append-returned body = %q, want %q", doc.Content, "line1\nline2")
	}
	got, err := s.Get("/log.md", 0)
	if err != nil {
		t.Fatalf("get after append: %v", err)
	}
	if body := string(got.Content); body != "line1\nline2" {
		t.Errorf("appended body = %q, want %q", body, "line1\nline2")
	}

	doc, err = s.Append("/log.md", 1, []byte("stale"), nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale append: err = %v, want ErrConflict", err)
	}
	if doc == nil || doc.Version != 2 {
		t.Errorf("stale append doc = %+v, want Version 2", doc)
	}
	if _, err := s.Append("/nope.md", 1, []byte("x"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("append missing: err = %v, want ErrNotExist", err)
	}
}

func testListDirSemantics(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/live.md", 0, "live")
	mustWrite(t, s, "/gone.md", 0, "gone")
	mustWrite(t, s, "/attic/old.md", 0, "old")
	mustWrite(t, s, "/nested/a/b/keep.md", 0, "keep")
	mustWrite(t, s, "/.hidden.md", 0, "hidden")
	mustWrite(t, s, "/work/.scratch.md", 0, "scratch")
	if err := s.Archive("/gone.md", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Archive("/attic/old.md", true); err != nil {
		t.Fatal(err)
	}

	hidden := entryNames(t, s, "/", false)
	for _, want := range []string{"live.md", "nested"} {
		if !hidden[want] {
			t.Errorf("hide view: want %s present, got %v", want, hidden)
		}
	}
	for _, absent := range []string{"gone.md", "attic", ".hidden.md", "work"} {
		if hidden[absent] {
			t.Errorf("hide view: want %s absent, got %v", absent, hidden)
		}
	}

	shown := entryNames(t, s, "/", true)
	for _, want := range []string{"live.md", "gone.md", "attic", "nested"} {
		if !shown[want] {
			t.Errorf("archived view: want %s present, got %v", want, shown)
		}
	}

	entries, err := s.ListEntries("/nested", false)
	if err != nil || len(entries) != 1 || entries[0].Name != "a" || !entries[0].IsDir {
		t.Errorf("nested list = (%+v, %v), want single dir 'a'", entries, err)
	}
	if _, err := s.ListEntries("/does-not-exist", false); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing dir: err = %v, want ErrNotExist", err)
	}
	if _, err := s.ListEntries("/live.md", false); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("doc path list: err = %v, want ErrNotExist", err)
	}
}

func testIsDirSemantics(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/dir/doc.md", 0, "x")

	if ok, err := s.IsDir("/"); err != nil || !ok {
		t.Errorf("IsDir(/) = (%v, %v), want true", ok, err)
	}
	if ok, err := s.IsDir("/dir"); err != nil || !ok {
		t.Errorf("IsDir(/dir) = (%v, %v), want true", ok, err)
	}
	if ok, err := s.IsDir("/dir/doc.md"); err != nil || ok {
		t.Errorf("IsDir(doc) = (%v, %v), want false", ok, err)
	}
	if _, err := s.IsDir("/missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("IsDir(missing): err = %v, want ErrNotExist", err)
	}
}

func testRetentionPrune(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/r.md", 0, "v1")
	mustWrite(t, s, "/r.md", 1, "v2")
	mustWrite(t, s, "/r.md", 2, "v3")
	doc, err := s.WriteVersion("/r.md", 3, []byte("v4"), map[string]string{"retention": "2"})
	if err != nil {
		t.Fatalf("retained write: %v", err)
	}
	if doc.Prune == nil || doc.Prune.From != 1 || doc.Prune.To != 2 {
		t.Errorf("prune = %+v, want From 1 To 2", doc.Prune)
	}
	vs, err := s.Versions("/r.md")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vs) != 2 || vs[0].Version != 4 || vs[1].Version != 3 {
		t.Errorf("surviving versions = %+v, want [4 3]", vs)
	}
	if err := s.VerifyChain("/r.md"); err != nil {
		t.Errorf("VerifyChain after prune: %v", err)
	}
	if _, err := s.Get("/r.md", 1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pruned version get: err = %v, want ErrNotExist", err)
	}
}

func testMissingDocument(t *testing.T, s handler.DocumentStore) {
	if _, err := s.Get("/none.md", 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Get missing: err = %v, want ErrNotExist", err)
	}
	if _, err := s.Versions("/none.md"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Versions missing: err = %v, want ErrNotExist", err)
	}
	if cur := s.CurrentVersion("/none.md"); cur != 0 {
		t.Errorf("CurrentVersion missing = %d, want 0", cur)
	}
	mustWrite(t, s, "/one.md", 0, "x")
	if _, err := s.Get("/one.md", 9); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Get absent version: err = %v, want ErrNotExist", err)
	}
}

func testPathCollisions(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/col/doc.md", 0, "x")

	// A document cannot be created where a directory exists. Both backends
	// must reject the write before persisting anything: no fetchable
	// document and no version state may remain.
	if _, err := s.WriteVersion("/col", 0, []byte("x"), nil); err == nil {
		t.Error("want error writing document over existing directory /col")
	}
	if _, err := s.Get("/col", 0); err == nil {
		t.Error("collision write left fetchable document at /col")
	}
	if cur := s.CurrentVersion("/col"); cur != 0 {
		t.Errorf("collision write left version state at /col (version %d)", cur)
	}

	// A document cannot be created beneath an existing document.
	if _, err := s.WriteVersion("/col/doc.md/child.md", 0, []byte("x"), nil); err == nil {
		t.Error("want error writing document under existing document /col/doc.md")
	}
	if _, err := s.Get("/col/doc.md/child.md", 0); err == nil {
		t.Error("collision write left fetchable document under /col/doc.md")
	}
	if cur := s.CurrentVersion("/col/doc.md/child.md"); cur != 0 {
		t.Errorf("collision write left version state under /col/doc.md (version %d)", cur)
	}
}

func testRejectsBinaryBody(t *testing.T, s handler.DocumentStore) {
	// A document body is markdown text; every backend must refuse to persist
	// non-UTF-8, even when called outside the network path.
	binary := []byte{0x89, 'P', 'N', 'G', 0xff, 0xfe, 0x00}

	if _, err := s.WriteVersion("/bin.md", 0, binary, nil); !errors.Is(err, store.ErrInvalidContent) {
		t.Errorf("WriteVersion binary body: err = %v, want ErrInvalidContent", err)
	}
	if _, err := s.Get("/bin.md", 0); err == nil {
		t.Error("rejected binary write left a fetchable document at /bin.md")
	}
	if cur := s.CurrentVersion("/bin.md"); cur != 0 {
		t.Errorf("rejected binary write left version state at /bin.md (version %d)", cur)
	}

	// An append that would introduce binary must be rejected too, leaving the
	// existing text version untouched.
	mustWrite(t, s, "/doc.md", 0, "# Text\n")
	if _, err := s.Append("/doc.md", 1, binary, nil); !errors.Is(err, store.ErrInvalidContent) {
		t.Errorf("Append binary body: err = %v, want ErrInvalidContent", err)
	}
	if cur := s.CurrentVersion("/doc.md"); cur != 1 {
		t.Errorf("rejected binary append advanced /doc.md to version %d, want 1", cur)
	}
}

func mustWrite(t *testing.T, s handler.DocumentStore, path string, expected int, body string) *store.Document {
	t.Helper()
	doc, err := s.WriteVersion(path, expected, []byte(body), nil)
	if err != nil {
		t.Fatalf("write %s (expected %d): %v", path, expected, err)
	}
	return doc
}

func entryNames(t *testing.T, s handler.DocumentStore, path string, includeArchived bool) map[string]bool {
	t.Helper()
	entries, err := s.ListEntries(path, includeArchived)
	if err != nil {
		t.Fatalf("list %s: %v", path, err)
	}
	m := map[string]bool{}
	for _, e := range entries {
		m[e.Name] = true
	}
	return m
}
