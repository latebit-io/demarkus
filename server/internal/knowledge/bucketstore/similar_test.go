package bucketstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
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

// Block mode waits for the index covering the gate's snapshot: a reworded
// copy published while the index lags behind the lesson is refused once it
// catches up; warn mode reads the newest index built and lets it land.
func TestBlockModeWaitsForTheCoveringIndex(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	gate := newGatedGets(memory)
	store := openSectionStore(t, gate, nil)
	tags := map[string]string{"tags": "store"}
	write(t, store, "/notes/other.md", neardtest.OtherDoc, tags)
	view, err := store.OpenReadView(ctx)
	mustSucceed(t, err)
	if hits := similarHits(t, view, catalog.Options{}); len(hits) != 0 {
		t.Fatalf("before the lesson = %+v, want none", hits)
	}
	mustSucceed(t, view.Close())

	gate.hold()
	write(t, store, "/notes/shards.md", neardtest.LessonDoc, tags)
	warning := backendtest.Direct{Store: writepolicy.Enforce(store, writepolicy.Options{Content: writepolicy.ContentOptions{NearDuplicate: contentcheck.Warn}})}
	if _, err := warning.WriteVersion("/notes/warned.md", 0, []byte(neardtest.RewordedDoc), tags); err != nil {
		t.Fatalf("warn mode behind the index = %v, want it to land", err)
	}
	blocking := backendtest.Direct{Store: writepolicy.Enforce(store, writepolicy.Options{})}
	refused := make(chan error, 1)
	go func() {
		_, err := blocking.WriteVersion("/notes/again.md", 0, []byte(neardtest.RewordedDoc), tags)
		refused <- err
	}()
	time.Sleep(50 * time.Millisecond)
	gate.release()
	if err := <-refused; !errors.Is(err, writepolicy.ErrNearDuplicate) {
		t.Fatalf("block mode behind the index = %v, want the refusal once it caught up", err)
	}
}
