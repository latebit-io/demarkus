// Package storetest is the DocumentStore conformance suite. Every backend
// (the file store, pgstore, any future one) must pass it unchanged:
// behavior divergence between backends is a protocol bug, not a backend
// detail. The file store runs it unconditionally; database-backed stores
// run it gated on their test DSN.
package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// Factory returns a fresh, empty DocumentStore for one conformance subtest.
type Factory func(t *testing.T) handler.DocumentStore

// Tamper overwrites the stored bytes of one version behind the store's back
// (a disk write, a raw UPDATE) so the suite can prove VerifyChain detects
// corruption on every backend.
type Tamper func(t testing.TB, s handler.DocumentStore, path string, version int, stored []byte)

// FileTamper is the file store's Tamper: it rewrites the version file.
func FileTamper(t testing.TB, s handler.DocumentStore, path string, version int, stored []byte) {
	t.Helper()
	fs, ok := s.(*store.Store)
	if !ok {
		t.Fatalf("FileTamper: store is %T, want *store.Store", s)
	}
	file, err := fs.VersionFilePath(path, version)
	if err != nil {
		t.Fatalf("FileTamper: %v", err)
	}
	if err := os.WriteFile(file, stored, 0o644); err != nil {
		t.Fatalf("FileTamper: %v", err)
	}
}

// RunConformance exercises the DocumentStore contract against the backend
// the factory produces. tamper is required: a backend that cannot be
// corrupted out of band cannot prove its chain verification works.
func RunConformance(t *testing.T, factory Factory, tamper Tamper) {
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
		{"AppendMetadataMerge", testAppendMetadataMerge},
		{"ListDirSemantics", testListDirSemantics},
		{"IsDirSemantics", testIsDirSemantics},
		{"Retention", testRetention},
		{"MissingDocument", testMissingDocument},
		{"PathCollisions", testPathCollisions},
		{"RejectsBinaryBody", testRejectsBinaryBody},
		{"SharedBodyHash", testSharedBodyHash},
		{"BodySizeLimit", testBodySizeLimit},
		{"InvalidMetadata", testInvalidMetadata},
		{"VerifyChainTampered", func(t *testing.T, s handler.DocumentStore) { testVerifyChainTampered(t, s, tamper) }},
		{"VerifyChainTamperedTip", func(t *testing.T, s handler.DocumentStore) { testVerifyChainTamperedTip(t, s, tamper) }},
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
	stored, err := store.SerializeVersion(1, nil, []byte("# First\n"), meta)
	if err != nil {
		t.Fatalf("serialize expected stored bytes: %v", err)
	}
	wantETag := store.StoredETag(stored)
	if doc.ETag != wantETag {
		t.Errorf("write ETag = %q, want raw stored-byte hash %q", doc.ETag, wantETag)
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
	if got.ETag != wantETag {
		t.Errorf("get ETag = %q, want raw stored-byte hash %q", got.ETag, wantETag)
	}
	// Contract: OKF fields (tags, type) and meta.-prefixed keys read back as
	// one map, tags in bare comma-separated form.
	want := map[string]string{"tags": "a,b", "importance": "0.5"}
	if !maps.Equal(doc.Metadata, want) || !maps.Equal(got.Metadata, want) {
		t.Errorf("metadata roundtrip: write %v get %v, want %v", doc.Metadata, got.Metadata, want)
	}
	plain := mustWrite(t, s, "/plain.md", 0, "# Hello\n")
	got, err = s.Get("/plain.md", 0)
	if err != nil {
		t.Fatalf("get plain: %v", err)
	}
	if plain.Metadata != nil || got.Metadata != nil {
		t.Errorf("nil metadata came back as write %v get %v", plain.Metadata, got.Metadata)
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
	first := mustWrite(t, s, "/doc.md", 0, "v1")
	mustWrite(t, s, "/doc.md", 1, "v2")
	mustWrite(t, s, "/doc.md", 2, "v3")

	if cur := currentVersion(t, s, "/doc.md"); cur != 3 {
		t.Errorf("CurrentVersion = %d, want 3", cur)
	}
	old, err := s.Get("/doc.md", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if string(old.Content) != "v1" {
		t.Errorf("v1 body = %q", old.Content)
	}
	if old.ETag == "" || old.ETag != first.ETag {
		t.Errorf("historical ETag = %q, want original %q", old.ETag, first.ETag)
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
	first, err := s.WriteVersion("/dup.md", 0, []byte("body"), meta)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	doc, err := s.WriteVersion("/dup.md", 1, []byte("body"), meta)
	if !errors.Is(err, store.ErrNotModified) {
		t.Fatalf("identical rewrite: err = %v, want ErrNotModified", err)
	}
	if doc == nil || doc.Version != 1 {
		t.Errorf("not-modified doc = %+v, want Version 1", doc)
	}
	if doc.ETag != first.ETag {
		t.Errorf("not-modified ETag = %q, want %q", doc.ETag, first.ETag)
	}
	if cur := currentVersion(t, s, "/dup.md"); cur != 1 {
		t.Errorf("CurrentVersion after dedup = %d, want 1", cur)
	}
	// Same body, different metadata is a real new version; so is dropping it.
	doc, err = s.WriteVersion("/dup.md", 1, []byte("body"), map[string]string{"tags": "changed"})
	if err != nil || doc.Version != 2 {
		t.Errorf("meta change = (%+v, %v), want v2", doc, err)
	}
	if doc.ETag == first.ETag {
		t.Error("metadata-only version reused prior stored-byte ETag")
	}
	secondETag := doc.ETag
	doc, err = s.WriteVersion("/dup.md", 2, []byte("body"), nil)
	if err != nil || doc.Version != 3 || doc.Metadata != nil {
		t.Errorf("meta drop = (%+v, %v), want v3 with nil metadata", doc, err)
	}
	if doc.ETag == secondETag {
		t.Error("metadata drop reused prior stored-byte ETag")
	}
}

func testArchiveLifecycle(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/arch.md", 0, "content")
	hash := store.ContentHash([]byte("content"))

	if p, err := s.LookupHash(hash); err != nil || p != "/arch.md" {
		t.Errorf("LookupHash before archive = (%q, %v), want (/arch.md, nil)", p, err)
	}
	archivedDoc := mustArchiveTransition(t, s, "/arch.md", true)
	if archivedDoc.Version != 1 {
		t.Errorf("archived version = %d, want 1", archivedDoc.Version)
	}
	unchangedDoc, changed, err := s.Archive("/arch.md", true)
	if err != nil || changed || unchangedDoc == nil || unchangedDoc.ETag != archivedDoc.ETag || !unchangedDoc.Modified.Equal(archivedDoc.Modified) {
		t.Errorf("archive no-op = (%+v, changed=%v, err=%v), want unchanged committed document", unchangedDoc, changed, err)
	}
	got, err := s.Get("/arch.md", 0)
	if err != nil {
		t.Fatalf("get archived: %v", err)
	}
	if !got.Archived {
		t.Error("archived flag not set on Get")
	}
	if _, err := s.LookupHash(hash); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LookupHash after archive: err = %v, want ErrNotExist", err)
	}
	if _, err := s.WriteVersion("/arch.md", 1, []byte("new"), nil); !errors.Is(err, store.ErrArchived) {
		t.Errorf("write to archived: err = %v, want ErrArchived", err)
	}
	unarchivedDoc := mustArchiveTransition(t, s, "/arch.md", false)
	if unarchivedDoc.Version != 1 {
		t.Errorf("unarchived version = %d, want 1", unarchivedDoc.Version)
	}
	if p, err := s.LookupHash(hash); err != nil || p != "/arch.md" {
		t.Errorf("LookupHash after unarchive = (%q, %v), want (/arch.md, nil)", p, err)
	}
	if doc, err := s.WriteVersion("/arch.md", 1, []byte("new"), nil); err != nil || doc.Version != 2 {
		t.Errorf("write after unarchive = (%+v, %v), want v2", doc, err)
	}
	if _, _, err := s.Archive("/missing.md", true); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("archive missing: err = %v, want ErrNotExist", err)
	}
}

func mustArchiveTransition(t *testing.T, s handler.DocumentStore, path string, archived bool) *store.Document {
	t.Helper()
	document, changed, err := s.Archive(path, archived)
	if err != nil {
		t.Fatalf("Archive(%q, %v): %v", path, archived, err)
	}
	if !changed || document == nil || document.Archived != archived {
		t.Fatalf("Archive(%q, %v) = (%+v, changed=%v), want changed document", path, archived, document, changed)
	}
	return document
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

// testAppendMetadataMerge covers SPEC 6.6: an append inherits the base
// version's publisher metadata, request keys winning. Without the merge the
// document loses its tags and falls out of LOOKUP.
func testAppendMetadataMerge(t *testing.T, s handler.DocumentStore) {
	seed := map[string]string{"tags": "alpha, beta", "importance": "0.9", "type": "Plan", "title": "Kept"}
	if _, err := s.WriteVersion("/tagged.md", 0, []byte("# Kept\n"), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got := appendMeta(t, s, "/tagged.md", 1, map[string]string{"agent": "claude"})
	if got["tags"] == "" || got["importance"] != "0.9" {
		t.Errorf("inherited metadata = %v, want tags and importance 0.9 preserved", got)
	}
	// The base version keeps what it was written with.
	if v1, err := s.Get("/tagged.md", 1); err != nil || v1.Metadata["agent"] != "" || v1.Metadata["title"] != "Kept" {
		t.Errorf("v1 metadata after append = %v (err %v), want the seed untouched", v1.Metadata, err)
	}
	if got["type"] != "Plan" {
		t.Errorf("type = %q, want Plan preserved (not overwritten by the OKF default)", got["type"])
	}
	if got["agent"] != "claude" {
		t.Errorf("agent = %q, want the request key applied", got["agent"])
	}

	// Request keys win over the base version's.
	got = appendMeta(t, s, "/tagged.md", 2, map[string]string{"importance": "0.2"})
	if got["importance"] != "0.2" || got["tags"] == "" {
		t.Errorf("override = %v, want importance 0.2 with tags still inherited", got)
	}

	// retention is never inherited: pruning is destructive and must be
	// declared by the write that performs it.
	if _, err := s.WriteVersion("/pruned.md", 0, []byte("v1"), map[string]string{"retention": "2", "tags": "x"}); err != nil {
		t.Fatalf("seed retention: %v", err)
	}
	got = appendMeta(t, s, "/pruned.md", 1, nil)
	if got["retention"] != "" {
		t.Errorf("retention = %q, want it dropped rather than inherited", got["retention"])
	}
	if got["tags"] != "x" {
		t.Errorf("tags = %q, want x inherited alongside the dropped retention", got["tags"])
	}

	// An untagged document stays untagged rather than gaining junk.
	mustWrite(t, s, "/plain.md", 0, "body")
	got = appendMeta(t, s, "/plain.md", 1, nil)
	if got["tags"] != "" || got["importance"] != "" {
		t.Errorf("untagged append metadata = %v, want no tags or importance", got)
	}

	// The merge can breach the caps both sides satisfy alone; that is a
	// metadata error, not a generic write failure.
	wide := map[string]string{"tags": strings.Repeat("x", protocol.MaxMetaBytes-32)}
	if _, err := s.WriteVersion("/wide.md", 0, []byte("v1"), wide); err != nil {
		t.Fatalf("seed wide: %v", err)
	}
	_, err := s.Append("/wide.md", 1, []byte("v2"), map[string]string{"title": strings.Repeat("y", 64)})
	if !errors.Is(err, store.ErrInvalidMeta) {
		t.Errorf("over-cap merge: err = %v, want ErrInvalidMeta", err)
	}
}

// appendMeta appends to a document and returns the stored metadata of the
// version the append produced.
func appendMeta(t *testing.T, s handler.DocumentStore, path string, expected int, meta map[string]string) map[string]string {
	t.Helper()
	if _, err := s.Append(path, expected, []byte("more"), meta); err != nil {
		t.Fatalf("append %s v%d: %v", path, expected, err)
	}
	doc, err := s.Get(path, 0)
	if err != nil {
		t.Fatalf("get %s after append: %v", path, err)
	}
	return doc.Metadata
}

func testListDirSemantics(t *testing.T, s handler.DocumentStore) {
	mustWrite(t, s, "/live.md", 0, "live")
	mustWrite(t, s, "/gone.md", 0, "gone")
	mustWrite(t, s, "/attic/old.md", 0, "old")
	mustWrite(t, s, "/nested/a/b/keep.md", 0, "keep")
	mustWrite(t, s, "/.hidden.md", 0, "hidden")
	mustWrite(t, s, "/work/.scratch.md", 0, "scratch")
	if _, _, err := s.Archive("/gone.md", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Archive("/attic/old.md", true); err != nil {
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

// testRetention pins retention: the window is applied on the write that
// carries it, numbering continues past pruned history, an unretained write
// stops pruning, and a window wider than the history is a no-op.
func testRetention(t *testing.T, s handler.DocumentStore) {
	doc := writeN(t, s, "/r.md", 4, map[string]string{"retention": "2"})
	if doc.Prune == nil || doc.Prune.From != 1 || doc.Prune.To != 2 || doc.Prune.Err != nil {
		t.Errorf("prune = %+v, want From 1 To 2 Err nil", doc.Prune)
	}
	if got, want := versionNums(t, s, "/r.md"), []int{4, 3}; !slices.Equal(got, want) {
		t.Errorf("surviving versions = %v, want %v", got, want)
	}
	if err := s.VerifyChain("/r.md"); err != nil {
		t.Errorf("VerifyChain after prune: %v", err)
	}
	if _, err := s.Get("/r.md", 1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pruned version get: err = %v, want ErrNotExist", err)
	}
	if cur, err := s.Get("/r.md", 0); err != nil || cur.Version != 4 {
		t.Errorf("current after prune = v%d (err %v), want v4", cur.Version, err)
	}

	if doc := writeN(t, s, "/none.md", 5, nil); doc.Prune != nil || len(versionNums(t, s, "/none.md")) != 5 {
		t.Errorf("no retention: prune = %+v, want nil and all 5 versions", doc.Prune)
	}

	writeN(t, s, "/one.md", 4, map[string]string{"retention": "1"})
	next, err := s.WriteVersion("/one.md", 4, []byte("# Version 5"), map[string]string{"retention": "1"})
	if err != nil {
		t.Fatalf("write after prune: %v", err)
	}
	if got := versionNums(t, s, "/one.md"); next.Version != 5 || !slices.Equal(got, []int{5}) {
		t.Errorf("retention 1: next = v%d, remaining = %v, want v5 and [5]", next.Version, got)
	}

	writeN(t, s, "/stop.md", 3, map[string]string{"retention": "2"})
	plain, err := s.WriteVersion("/stop.md", 3, []byte("# Version 4"), nil)
	if err != nil {
		t.Fatalf("unretained write: %v", err)
	}
	if got := versionNums(t, s, "/stop.md"); plain.Prune != nil || !slices.Equal(got, []int{4, 3, 2}) {
		t.Errorf("retention removed: prune = %+v, remaining = %v, want nil and [4 3 2]", plain.Prune, got)
	}

	if doc := writeN(t, s, "/wide.md", 3, map[string]string{"retention": "10"}); doc.Prune != nil || len(versionNums(t, s, "/wide.md")) != 3 {
		t.Errorf("retention wider than history: prune = %+v, want nil and all 3 versions", doc.Prune)
	}
}

func testMissingDocument(t *testing.T, s handler.DocumentStore) {
	if _, err := s.Get("/none.md", 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Get missing: err = %v, want ErrNotExist", err)
	}
	if _, err := s.Versions("/none.md"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Versions missing: err = %v, want ErrNotExist", err)
	}
	if cur := currentVersion(t, s, "/none.md"); cur != 0 {
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
	if cur := currentVersion(t, s, "/col"); cur != 0 {
		t.Errorf("collision write left version state at /col (version %d)", cur)
	}

	// A document cannot be created beneath an existing document.
	if _, err := s.WriteVersion("/col/doc.md/child.md", 0, []byte("x"), nil); err == nil {
		t.Error("want error writing document under existing document /col/doc.md")
	}
	if _, err := s.Get("/col/doc.md/child.md", 0); err == nil {
		t.Error("collision write left fetchable document under /col/doc.md")
	}
	if cur := currentVersion(t, s, "/col/doc.md/child.md"); cur != 0 {
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
	if cur := currentVersion(t, s, "/bin.md"); cur != 0 {
		t.Errorf("rejected binary write left version state at /bin.md (version %d)", cur)
	}

	// An append that would introduce binary must be rejected too, leaving the
	// existing text version untouched.
	mustWrite(t, s, "/doc.md", 0, "# Text\n")
	if _, err := s.Append("/doc.md", 1, binary, nil); !errors.Is(err, store.ErrInvalidContent) {
		t.Errorf("Append binary body: err = %v, want ErrInvalidContent", err)
	}
	if cur := currentVersion(t, s, "/doc.md"); cur != 1 {
		t.Errorf("rejected binary append advanced /doc.md to version %d, want 1", cur)
	}
}

// testSharedBodyHash pins LookupHash when several live documents share a
// body: the smallest path wins, and updating or archiving one never drops
// the others.
func testSharedBodyHash(t *testing.T, s handler.DocumentStore) {
	hash := store.ContentHash([]byte("same"))
	mustWrite(t, s, "/b.md", 0, "same")
	mustWrite(t, s, "/a.md", 0, "same")
	mustWrite(t, s, "/c.md", 0, "same")
	if p, err := s.LookupHash(hash); err != nil || p != "/a.md" {
		t.Errorf("LookupHash shared = (%q, %v), want (/a.md, nil)", p, err)
	}
	if _, _, err := s.Archive("/a.md", true); err != nil {
		t.Fatal(err)
	}
	if p, err := s.LookupHash(hash); err != nil || p != "/b.md" {
		t.Errorf("LookupHash after archiving /a.md = (%q, %v), want (/b.md, nil)", p, err)
	}
	mustWrite(t, s, "/b.md", 1, "changed")
	if p, err := s.LookupHash(hash); err != nil || p != "/c.md" {
		t.Errorf("LookupHash after rewriting /b.md = (%q, %v), want (/c.md, nil)", p, err)
	}
	if p, err := s.LookupHash(store.ContentHash([]byte("changed"))); err != nil || p != "/b.md" {
		t.Errorf("LookupHash new body = (%q, %v), want (/b.md, nil)", p, err)
	}
	if _, _, err := s.Archive("/a.md", false); err != nil {
		t.Fatal(err)
	}
	if p, err := s.LookupHash(hash); err != nil || p != "/a.md" {
		t.Errorf("LookupHash after unarchiving /a.md = (%q, %v), want (/a.md, nil)", p, err)
	}
}

// testBodySizeLimit pins the body boundary: exactly MaxBodyLength is stored,
// one byte more is ErrSizeLimit, and an append whose joined content crosses
// the limit is rejected without advancing the version.
func testBodySizeLimit(t *testing.T, s handler.DocumentStore) {
	atLimit := bytes.Repeat([]byte("x"), protocol.MaxBodyLength)
	if _, err := s.WriteVersion("/max.md", 0, atLimit, nil); err != nil {
		t.Fatalf("write at limit: %v", err)
	}
	if _, err := s.WriteVersion("/over.md", 0, append(atLimit, 'x'), nil); !errors.Is(err, store.ErrSizeLimit) {
		t.Errorf("write over limit: err = %v, want ErrSizeLimit", err)
	}
	if cur := currentVersion(t, s, "/over.md"); cur != 0 {
		t.Errorf("over-limit write left version %d", cur)
	}
	if _, err := s.Append("/max.md", 1, []byte("y"), nil); !errors.Is(err, store.ErrSizeLimit) {
		t.Errorf("append crossing limit: err = %v, want ErrSizeLimit", err)
	}
	if cur := currentVersion(t, s, "/max.md"); cur != 1 {
		t.Errorf("rejected append advanced /max.md to version %d, want 1", cur)
	}
}

// testInvalidMetadata pins the metadata rejections applied before anything is
// stored: reserved keys, malformed keys and values, bad retention values, and
// tags whose serialized form overflows although the comma-separated value fits.
func testInvalidMetadata(t *testing.T, s handler.DocumentStore) {
	bigTags := strings.Repeat("a,", 399) + "a"
	if len("tags")+len(bigTags) > protocol.MaxMetaBytes || store.SerializedMetaSize("tags", bigTags) <= protocol.MaxMetaBytes {
		t.Fatalf("test setup: tags must fit as csv and overflow serialized")
	}
	cases := []struct {
		name string
		meta map[string]string
	}{
		{"reserved version", map[string]string{"version": "x"}},
		{"reserved archived", map[string]string{"archived": "x"}},
		{"reserved previous-hash", map[string]string{"previous-hash": "x"}},
		{"uppercase key", map[string]string{"Type": "note"}},
		{"underscore key", map[string]string{"my_key": "val"}},
		{"empty key", map[string]string{"": "val"}},
		{"newline in value", map[string]string{"type": "note\nevil: injected"}},
		{"retention zero", map[string]string{"retention": "0"}},
		{"retention negative", map[string]string{"retention": "-3"}},
		{"retention non-integer", map[string]string{"retention": "abc"}},
		{"retention float", map[string]string{"retention": "1.5"}},
		{"retention empty", map[string]string{"retention": ""}},
		{"serialized tags over cap", map[string]string{"tags": bigTags}},
	}
	mustWrite(t, s, "/base.md", 0, "# Base\n")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.WriteVersion("/inv.md", 0, []byte("# Hi\n"), c.meta); !errors.Is(err, store.ErrInvalidMeta) {
				t.Errorf("write: err = %v, want ErrInvalidMeta", err)
			}
			if _, err := s.Append("/base.md", 1, []byte("x"), c.meta); !errors.Is(err, store.ErrInvalidMeta) {
				t.Errorf("append: err = %v, want ErrInvalidMeta", err)
			}
		})
	}
	// Nothing was stored by the rejected writes.
	if cur := currentVersion(t, s, "/inv.md"); cur != 0 {
		t.Errorf("rejected writes left version %d", cur)
	}
	if cur := currentVersion(t, s, "/base.md"); cur != 1 {
		t.Errorf("rejected appends advanced /base.md to version %d", cur)
	}
	// Valid retention values are accepted.
	for _, v := range []string{"1", "20"} {
		if _, err := s.WriteVersion("/ok.md", -1, []byte(v), map[string]string{"retention": v}); err != nil {
			t.Errorf("retention %q rejected: %v", v, err)
		}
	}
}

// testVerifyChainTamperedTip corrupts the newest version itself. Its stored
// bytes carry the previous-hash link, so a backend that trusted a recorded
// hash instead of the bytes would miss this.
func testVerifyChainTamperedTip(t *testing.T, s handler.DocumentStore, tamper Tamper) {
	writeN(t, s, "/dir/tip.md", 2, nil)
	tamper(t, s, "/dir/tip.md", 2, []byte("# TAMPERED\n"))
	err := s.VerifyChain("/dir/tip.md")
	if err == nil || !strings.Contains(err.Error(), "missing previous-hash") {
		t.Errorf("VerifyChain after tip tamper: err = %v, want missing previous-hash", err)
	}
}

// testVerifyChainTampered corrupts v1 after v2 links to it: VerifyChain must
// report a broken chain while Versions still lists both.
func testVerifyChainTampered(t *testing.T, s handler.DocumentStore, tamper Tamper) {
	writeN(t, s, "/dir/t.md", 2, nil)
	if err := s.VerifyChain("/dir/t.md"); err != nil {
		t.Fatalf("VerifyChain before tamper: %v", err)
	}
	tamper(t, s, "/dir/t.md", 1, []byte("# TAMPERED\n"))
	err := s.VerifyChain("/dir/t.md")
	if err == nil || !strings.Contains(err.Error(), "chain broken") {
		t.Errorf("VerifyChain after tamper: err = %v, want chain broken", err)
	}
	if got := versionNums(t, s, "/dir/t.md"); !slices.Equal(got, []int{2, 1}) {
		t.Errorf("Versions after tamper = %v, want [2 1]", got)
	}
}

// writeN writes n versions of path, passing meta only on the last write,
// and returns the final document.
func writeN(t *testing.T, s handler.DocumentStore, path string, n int, finalMeta map[string]string) *store.Document {
	t.Helper()
	var doc *store.Document
	for i := 1; i <= n; i++ {
		var meta map[string]string
		if i == n {
			meta = finalMeta
		}
		var err error
		if doc, err = s.WriteVersion(path, i-1, fmt.Appendf(nil, "# Version %d", i), meta); err != nil {
			t.Fatalf("write %s v%d: %v", path, i, err)
		}
	}
	return doc
}

// versionNums returns the surviving version numbers, newest first.
func versionNums(t *testing.T, s handler.DocumentStore, path string) []int {
	t.Helper()
	infos, err := s.Versions(path)
	if err != nil {
		t.Fatalf("versions %s: %v", path, err)
	}
	nums := make([]int, 0, len(infos))
	for _, v := range infos {
		nums = append(nums, v.Version)
	}
	return nums
}

func mustWrite(t *testing.T, s handler.DocumentStore, path string, expected int, body string) *store.Document {
	t.Helper()
	doc, err := s.WriteVersion(path, expected, []byte(body), nil)
	if err != nil {
		t.Fatalf("write %s (expected %d): %v", path, expected, err)
	}
	return doc
}

func currentVersion(t testing.TB, s handler.DocumentStore, path string) int {
	t.Helper()
	version, err := s.CurrentVersion(path)
	if err != nil {
		t.Fatalf("CurrentVersion(%s): %v", path, err)
	}
	return version
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
