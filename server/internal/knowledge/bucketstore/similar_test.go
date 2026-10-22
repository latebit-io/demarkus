package bucketstore

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// similarHits is one similar-mode lookup of the reworded lesson on a view.
func similarHits(t *testing.T, view backend.ReadView, opts catalog.Options) []catalog.Result {
	t.Helper()
	draft := neardup.AnalyzeWhole(neardtest.RewordedDoc)
	opts.Match, opts.Draft = catalog.MatchSimilar, &draft
	hits, err := view.Lookup(context.Background(), "", opts)
	mustSucceed(t, err)
	return hits
}

// Similar mode answers from the same section index version as body lookup;
// stale answers from the newest built without waiting on the snapshot.
func TestSimilarAnswersFromTheSectionIndex(t *testing.T) {
	ctx := context.Background()
	store := openSectionStore(t, initializedMemory(t), nil)
	write(t, store, "/notes/shards.md", neardtest.LessonDoc, map[string]string{"tags": "store"})
	write(t, store, "/notes/tenants.md", neardtest.OtherDoc, nil)
	assertRows(t, store, "compactor", "/notes/shards.md#shards")

	view, err := store.OpenReadView(ctx)
	mustSucceed(t, err)
	defer func() { mustSucceed(t, view.Close()) }()
	if hits := similarHits(t, view, catalog.Options{}); len(hits) != 1 || hits[0].Path != "/notes/shards.md" || hits[0].Hit.Verdict != neardup.VerdictNearDuplicate {
		t.Fatalf("hits = %+v, want the shards note as a near-duplicate", hits)
	}
	if hits := similarHits(t, view, catalog.Options{Skip: func(path string) bool { return path == "/notes/shards.md" }}); len(hits) != 0 {
		t.Errorf("hits with the note skipped = %+v", hits)
	}
	if hits := similarHits(t, view, catalog.Options{Stale: true}); len(hits) != 1 {
		t.Errorf("stale similar = %+v, want the same hit from the newest index", hits)
	}
}

// A store warmed at open on a world that already holds the lesson judges
// its very first write: the stale read waits for the build instead of
// passing the write unchecked.
func TestFirstWriteAfterOpenIsJudged(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	write(t, writer, "/notes/shards.md", neardtest.LessonDoc, map[string]string{"tags": "store"})
	mustSucceed(t, writer.checkpoint(ctx))

	fresh := openSectionStore(t, memory, nil)
	fresh.WarmSections()
	if !fresh.sections.isActive() {
		t.Fatal("WarmSections did not start the index")
	}
	view, err := fresh.OpenReadView(ctx)
	mustSucceed(t, err)
	defer func() { mustSucceed(t, view.Close()) }()
	if hits := similarHits(t, view, catalog.Options{Stale: true}); len(hits) != 1 || hits[0].Path != "/notes/shards.md" {
		t.Fatalf("first stale similar lookup after open = %+v, want the lesson", hits)
	}
	ro := openSectionStore(t, memory, func(o *Options) { o.ReadOnly = true })
	ro.WarmSections()
	if ro.sections.isActive() {
		t.Error("a read-only store warmed its section index")
	}
}
