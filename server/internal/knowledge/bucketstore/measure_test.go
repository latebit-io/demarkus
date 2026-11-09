package bucketstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/backend"
)

// refold is a never-checkpointed world, whose first checkpoint folds every
// shard: its writer, snapshot, count-rule layout and folded history.
type refold struct {
	store   *Store
	writer  checkpointWriter
	snap    *snapshot
	layout  shardLayout
	entries map[string]*baseEntry
}

func refoldWorld(tb testing.TB, documents int) *refold {
	tb.Helper()
	ctx := context.Background()
	objects := initializedMemoryOf(tb, MaxObjectBytes)
	store, err := Open(ctx, objects, Options{Logger: discardLogger, WorldID: testWorldID, trigger: manual})
	if err != nil {
		tb.Fatalf("open: %v", err)
	}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Errorf("close: %v", err)
		}
	})
	// Concurrent publishes batch into few slots, keeping setup short.
	err = runParallel(ctx, 64, indexes(documents), func(ctx context.Context, i int) error {
		path, content, metadata := refoldDocument(i)
		_, err := store.Publish(ctx, backend.WriteRequest{Path: path, Content: content, Metadata: metadata})
		return err
	})
	if err != nil {
		tb.Fatalf("publish: %v", err)
	}
	snap := store.served.Load().snap
	if snap.Paths.Len() != documents {
		tb.Fatalf("world holds %d documents, want %d", snap.Paths.Len(), documents)
	}
	writer := store.checkpointWriter()
	layout := writer.group(snap, shardBitsFor(documents))
	entries, _, err := writer.foldChanged(ctx, layout.changed)
	if err != nil {
		tb.Fatalf("fold history: %v", err)
	}
	return &refold{store: store, writer: writer, snap: snap, layout: layout, entries: entries}
}

var (
	refoldAreas = []string{"decisions", "design", "runbooks", "journal", "notes", "incidents"}
	refoldKinds = []string{"decision", "design", "runbook", "journal", "note"}
)

// refoldDocument is a document as agents publish them: nested path, tags,
// importance, a type, a description and a pinned source. Some carry text
// JSON escapes, so a size that skipped an escape would drift.
func refoldDocument(i int) (path string, content []byte, metadata map[string]string) {
	area := refoldAreas[i%len(refoldAreas)]
	path = fmt.Sprintf("/projects/project-%02d/%s/topic-%05d-%s.md", i%40, area, i, strings.Repeat("x", i%24))
	content = fmt.Appendf(nil, "# Topic %d in %s\n\n## Context\n\nWhy topic %d exists.\n\n## Decision\n\nWhat was decided.\n", i, area, i)
	description := fmt.Sprintf("How topic %d in %s shapes the bucket store's folded checkpoint and what a reader pays when a shard is rewritten.", i, area)
	if i%5 == 0 {
		description += " Escapes: <b>&amp;</b> \"quoted\" back\\slash é 🚀 \u2028 line separator."
	}
	metadata = map[string]string{
		"tags":         fmt.Sprintf("storage,checkpoint,%s,team-%d", area, i%7),
		"importance":   fmt.Sprintf("0.%d", 3+i%6),
		"type":         refoldKinds[i%len(refoldKinds)],
		"description":  description,
		"rel-based-on": fmt.Sprintf("https://github.com/latebit-io/demarkus/tree/%040x/server/internal/knowledge/bucketstore/checkpoint.go", i),
	}
	return path, content, metadata
}

// A measured size is the byte length the checkpoint then writes, for every
// shard, so measure and create cannot drift.
func TestMeasuredSizeIsTheWrittenSize(t *testing.T) {
	world := refoldWorld(t, 3*docsPerShard)
	mustSucceed(t, world.store.checkpoint(context.Background()))
	_, root := readFoldedRoot(t, world.store.objects)
	if root.ShardBits != world.layout.bits || len(world.layout.shards) != len(root.Shards) {
		t.Fatalf("checkpoint wrote %d shards at %d bits, want all %d at %d", len(root.Shards), root.ShardBits, 1<<world.layout.bits, world.layout.bits)
	}
	for _, index := range world.layout.shards {
		size, err := world.layout.size(index, world.entries)
		mustSucceed(t, err)
		if written := len(getObject(t, world.store.objects, root.Shards[index].Key).Data); size != written {
			t.Fatalf("shard %s measured %d bytes, written %d", root.Shards[index].Shard, size, written)
		}
	}
}

// exactSizes folds every shard at every width, as the create pass does, and
// checks size against it; sizes[bits][index] is the shard's length.
func exactSizes(t *testing.T, world *refold) [][]int {
	t.Helper()
	sizes := make([][]int, maxShardBits+1)
	for bits := range sizes {
		layout := world.writer.group(world.snap, bits)
		sizes[bits] = make([]int, 1<<bits)
		for _, index := range layout.shards {
			object, _, err := layout.fold(index, world.entries)
			mustSucceed(t, err)
			size, err := layout.size(index, world.entries)
			mustSucceed(t, err)
			if size != len(object.Data) {
				t.Fatalf("shard %s measured %d bytes, folded %d", shardLabel(index, bits), size, len(object.Data))
			}
			sizes[bits][index] = size
		}
	}
	return sizes
}

// A shard holds at most its parent's bytes plus headerGrowth, exactly that
// when it holds all of the parent's documents, which lets measure bound it.
func TestShardWithinItsParentAndHeaderGrowth(t *testing.T) {
	world := refoldWorld(t, 300)
	sizes := exactSizes(t, world)
	for bits := 1; bits <= maxShardBits; bits++ {
		growth, err := headerGrowth(bits)
		mustSucceed(t, err)
		narrow, wide := world.writer.group(world.snap, bits-1), world.writer.group(world.snap, bits)
		for index, size := range sizes[bits] {
			parent := sizes[bits-1][index>>1]
			whole := len(wide.groups[index]) == len(narrow.groups[index>>1])
			if size > parent+growth || whole && size != parent+growth {
				t.Fatalf("shard %s of %d documents is %d bytes; parent of %d is %d, growth %d", shardLabel(index, bits), len(wide.groups[index]), size, len(narrow.groups[index>>1]), parent, growth)
			}
		}
	}
}

// fit settles on the narrowest width whose shards all fit, as folding every
// width says, whether a measure stops early or bounds a shard by its parent.
func TestFitPicksTheNarrowestLayoutThatFits(t *testing.T) {
	world := refoldWorld(t, 300)
	sizes := exactSizes(t, world)
	start := world.layout.bits
	largest := make([]int, len(sizes))
	for bits, widths := range sizes {
		largest[bits] = slices.Max(widths)
	}
	for _, workers := range []int{1, defaultShardWorkers} {
		for bits := start; bits <= maxShardBits; bits++ {
			for _, budget := range []int{largest[bits], largest[bits] - 1} {
				want := -1
				for width := start; width <= maxShardBits && want < 0; width++ {
					if largest[width] <= budget {
						want = width
					}
				}
				writer := world.writer
				writer.workers, writer.budget = workers, budget
				layout, err := writer.fit(context.Background(), world.snap, world.layout, world.entries)
				switch {
				case want < 0 && (err == nil || !strings.Contains(err.Error(), "exceeds")):
					t.Fatalf("%d workers, budget %d: fit = %d bits, %v; want an oversized shard", workers, budget, layout.bits, err)
				case want >= 0 && (err != nil || layout.bits != want):
					t.Fatalf("%d workers, budget %d: fit = %d bits, %v; want %d", workers, budget, layout.bits, err, want)
				}
			}
		}
	}
}

// BenchmarkFullRefold is the shard work of a checkpoint that rewrites every
// shard: fitting the layout to the budget, then folding each shard to create.
// widen sets a budget most count-rule shards exceed, so the layout widens once.
func BenchmarkFullRefold(b *testing.B) {
	world := refoldWorld(b, 32*docsPerShard)
	b.Run("count", func(b *testing.B) { benchmarkRefold(b, world, world.layout.bits) })
	largest := 0
	for _, index := range world.layout.shards {
		object, _, err := world.layout.fold(index, world.entries)
		if err != nil {
			b.Fatal(err)
		}
		largest = max(largest, len(object.Data))
	}
	world.writer.budget = largest * 3 / 4
	b.Run("widen", func(b *testing.B) { benchmarkRefold(b, world, world.layout.bits+1) })
}

func benchmarkRefold(b *testing.B, world *refold, bits int) {
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		layout, err := world.writer.fit(ctx, world.snap, world.layout, world.entries)
		if err != nil {
			b.Fatal(err)
		}
		if layout.bits != bits {
			b.Fatalf("layout fit at %d bits, want %d", layout.bits, bits)
		}
		err = runParallel(ctx, world.writer.workers, layout.shards, func(_ context.Context, index int) error {
			_, _, err := layout.fold(index, world.entries)
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
