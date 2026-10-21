package bucketstore

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// Similar mode answers from the same section index version as body lookup;
// stale answers from the newest built without waiting.
func TestSimilarAnswersFromTheSectionIndex(t *testing.T) {
	ctx := context.Background()
	store := openSectionStore(t, initializedMemory(t), nil)
	write(t, store, "/notes/shards.md", neardtest.LessonDoc, map[string]string{"tags": "store"})
	write(t, store, "/notes/tenants.md", neardtest.OtherDoc, nil)
	assertRows(t, store, "compactor", "/notes/shards.md#shards")

	view, err := store.OpenReadView(ctx)
	mustSucceed(t, err)
	defer func() { mustSucceed(t, view.Close()) }()
	draft := neardup.AnalyzeWhole(neardtest.RewordedDoc)
	opts := catalog.Options{Match: catalog.MatchSimilar, Draft: &draft}
	hits, err := view.Lookup(ctx, "", opts)
	mustSucceed(t, err)
	if len(hits) != 1 || hits[0].Path != "/notes/shards.md" || hits[0].Hit.Verdict != neardup.VerdictNearDuplicate {
		t.Fatalf("hits = %+v, want the shards note as a near-duplicate", hits)
	}
	opts.Skip = func(path string) bool { return path == "/notes/shards.md" }
	hits, err = view.Lookup(ctx, "", opts)
	mustSucceed(t, err)
	if len(hits) != 0 {
		t.Errorf("hits with the note skipped = %+v", hits)
	}
	opts.Skip, opts.Stale = nil, true
	if hits, err := view.Lookup(ctx, "", opts); err != nil || len(hits) != 1 {
		t.Errorf("stale similar = %+v, %v; want the same hit from the newest index", hits, err)
	}
}
