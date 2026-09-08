package bucketstore

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// bodyRows runs a body-mode lookup and returns path#anchor keys.
func bodyRows(t *testing.T, store *Store, query string) []string {
	t.Helper()
	rs, err := store.Lookup(query, catalog.Options{Match: catalog.MatchBody})
	if err != nil {
		t.Fatalf("lookup %q: %v", query, err)
	}
	keys := make([]string, len(rs))
	for i := range rs {
		keys[i] = rs[i].Location()
	}
	return keys
}

// TestSectionIndexAcrossSnapshots proves the index is built at Open from
// the bucket, carried by body hash on refresh, and rebuilt for a changed
// document, so a second replica answers body match without a write of its own.
func TestSectionIndexAcrossSnapshots(t *testing.T) {
	writer, objects := newWritableStore(t)
	if _, err := writer.WriteVersion("/docs/a.md", 0, []byte("# A\n\n## Hairpin\n\nnat on the same host\n"), map[string]string{"tags": "net"}); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if _, err := writer.WriteVersion("/docs/b.md", 0, []byte("# B\n\nkqueue symlink swap\n"), nil); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if got := strings.Join(bodyRows(t, writer, "hairpin"), ","); got != "/docs/a.md#hairpin" {
		t.Fatalf("writer body rows = %q", got)
	}

	reader, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	reader.commitInterval = 0
	if got := strings.Join(bodyRows(t, reader, "kqueue"), ","); got != "/docs/b.md#b" {
		t.Errorf("reader after open = %q, want the index built from the bucket", got)
	}
	carried := reader.snapshot.Load().Catalog.Sections("/docs/a.md")

	// Change b through the writer; the reader's refresh reindexes b only.
	if _, err := writer.WriteVersion("/docs/b.md", 1, []byte("# B\n\n## Poison\n\nlock pid write\n"), nil); err != nil {
		t.Fatalf("rewrite b: %v", err)
	}
	if got := strings.Join(bodyRows(t, reader, "poison"), ","); got != "/docs/b.md#poison" {
		t.Errorf("reader after refresh = %q, want the rewritten section", got)
	}
	if len(bodyRows(t, reader, "kqueue")) != 0 {
		t.Error("stale section survived the rewrite")
	}
	if reader.snapshot.Load().Catalog.Sections("/docs/a.md") != carried {
		t.Error("unchanged document was reindexed instead of carried by body hash")
	}

	// Archive drops the sections; unarchive rebuilds them from the bucket.
	if _, _, err := writer.ArchiveResult("/docs/a.md", true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if len(bodyRows(t, reader, "hairpin")) != 0 || len(bodyRows(t, writer, "hairpin")) != 0 {
		t.Error("archived document still matches")
	}
	if _, _, err := writer.ArchiveResult("/docs/a.md", false); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if got := strings.Join(bodyRows(t, writer, "hairpin"), ","); got != "/docs/a.md#hairpin" {
		t.Errorf("writer after unarchive = %q", got)
	}
}
