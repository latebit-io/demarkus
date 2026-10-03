package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// manual keeps compactors from running on their own, so a test checkpoints
// where it says.
var manual = &compactionTrigger{slots: math.MaxInt, age: time.Duration(math.MaxInt64), wait: func() time.Duration { return 0 }}

func manualSite(objects blob.Store) *bucketSite {
	return &bucketSite{objects: objects, trigger: manual}
}

func mustSucceed(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// writeWorld writes one round of every kind of change: creates and updates
// under directories, an append, an archive toggle, and retention pruning.
func writeWorld(t *testing.T, store *Store, round int) {
	t.Helper()
	for index, path := range []string{"/a.md", "/docs/b.md", "/docs/deep/c.md", "/.hidden/d.md", "/docs/versions/e.md", fmt.Sprintf("/round/%d.md", round)} {
		_, err := store.WriteVersion(path, -1, fmt.Appendf(nil, "# %d.%d\n\n## Part\n\nbody %d\n", round, index, index), map[string]string{"tags": "checkpoint"})
		mustSucceed(t, err)
	}
	current, err := store.CurrentVersionResult("/docs/b.md")
	mustSucceed(t, err)
	_, err = store.AppendVersion("/docs/b.md", current, fmt.Appendf(nil, "more %d\n", round), nil)
	mustSucceed(t, err)
	if round == 0 {
		_, err = store.WriteVersion("/archive/x.md", 0, []byte("# X\n"), nil)
		mustSucceed(t, err)
	}
	_, _, err = store.ArchiveResult("/archive/x.md", round%2 == 0)
	mustSucceed(t, err)
	for range 4 {
		_, err = store.WriteVersion("/retained.md", -1, fmt.Appendf(nil, "v%d %d", round, time.Now().UnixNano()), map[string]string{"retention": "3"})
		mustSucceed(t, err)
	}
}

// worldDigest is what a reader sees of the served snapshot, every document
// with its full history, however checkpoint and log split it.
func worldDigest(t *testing.T, store *Store) digest {
	t.Helper()
	snap := store.served.Load().snap
	view := &readView{objects: store.objects, snapshot: snap}
	d := snapshotDigest(snap)
	d.paths = nil
	snap.Paths.Ascend(func(state *pathState) bool {
		history, err := view.history(context.Background(), state)
		if err != nil {
			t.Fatalf("history of %s: %v", state.Path, err)
		}
		versions := make([]string, 0, len(history))
		for _, version := range history {
			versions = append(versions, fmt.Sprintf("%d:%s:%s:%s", version.entry.Version, version.entry.Blob.Hash, version.entry.BodyHash, version.modified))
		}
		d.paths = append(d.paths, fmt.Sprintf("%s current=%d archived=%t body=%s modified=%s indexed=%t entry=%+v versions=%v",
			state.Path, state.Current, state.Archived, state.BodyHash, state.Modified, state.Sections != nil, *state.Entry, versions))
		return true
	})
	return d
}

// A checkpoint holds exactly what replaying the log to it does: a replica
// that loads it and replays the rest, the writer that rebased on it, and a
// replica that replays everything from checkpoint zero agree.
func TestCheckpointEqualsReplay(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := manualSite(objects)
	writer := site.open(t, 0)
	writeWorld(t, writer, 0)
	mustSucceed(t, writer.checkpoint(ctx))
	writeWorld(t, writer, 1)
	checkpointed := writer.served.Load().snap.Sequence
	mustSucceed(t, writer.checkpoint(ctx))
	writeWorld(t, writer, 2)

	served := writer.served.Load().snap
	if served.Checkpoint.Sequence != checkpointed {
		t.Fatalf("writer rests on checkpoint %d, want %d", served.Checkpoint.Sequence, checkpointed)
	}
	served.Paths.Ascend(func(state *pathState) bool {
		for _, version := range state.Recent {
			if state.Base != nil && version.entry.Version <= state.Base.Current {
				t.Errorf("%s holds v%d in memory, which checkpoint %d has", state.Path, version.entry.Version, checkpointed)
			}
		}
		return true
	})

	live := worldDigest(t, writer)
	cold := site.open(t, 0)
	if sequence := cold.served.Load().snap.Checkpoint.Sequence; sequence != checkpointed {
		t.Fatalf("cold start loaded checkpoint %d, want %d", sequence, checkpointed)
	}
	if got := worldDigest(t, cold); !reflect.DeepEqual(live, got) {
		t.Errorf("checkpoint plus replay differs from the live snapshot:\nlive %+v\ncold %+v", live, got)
	}
	for _, sequence := range []int64{checkpointed, 2} {
		deleteNewer(t, objects, sequence)
	}
	if got := worldDigest(t, site.open(t, 0)); !reflect.DeepEqual(live, got) {
		t.Errorf("replay from checkpoint zero differs from the live snapshot:\nlive %+v\ncold %+v", live, got)
	}
}

// deleteNewer deletes every checkpoint from sequence on.
func deleteNewer(t *testing.T, objects blob.Store, sequence int64) {
	t.Helper()
	for sequences, err := range sequencePages(context.Background(), objects, checkpointPrefix, sequence-1) {
		mustSucceed(t, err)
		for _, newer := range sequences {
			deleteObject(t, objects, checkpointKey(newer))
		}
	}
}

// Two compactors that checkpoint one sequence from different bases, one
// rebased on an earlier checkpoint and one on checkpoint zero, write the same
// bytes; create-if-absent refuses any that differ.
func TestRacingCompactorsWriteIdenticalBytes(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := manualSite(objects)
	writer, peer := site.open(t, 0), site.open(t, 0)
	writeWorld(t, writer, 0)
	mustSucceed(t, writer.checkpoint(ctx))
	writeWorld(t, peer, 1)
	mustSucceed(t, writer.poll(ctx))
	mustSucceed(t, peer.poll(ctx))
	first, second := writer.served.Load().snap, peer.served.Load().snap
	if first.Sequence != second.Sequence || first.Checkpoint.Sequence == second.Checkpoint.Sequence {
		t.Fatalf("snapshots at %d and %d on checkpoints %d and %d, want one sequence on two bases",
			first.Sequence, second.Sequence, first.Checkpoint.Sequence, second.Checkpoint.Sequence)
	}
	var wg sync.WaitGroup
	results := make([]*adoption, 2)
	errs := make([]error, 2)
	for index, snap := range []*snapshot{first, second} {
		wg.Go(func() {
			results[index], errs[index] = checkpointWriter{objects: objects, worldID: testWorldID, workers: 4}.write(ctx, snap)
		})
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("compactor %d: %v", index, err)
		}
	}
	if !reflect.DeepEqual(results[0].checkpoint, results[1].checkpoint) {
		t.Errorf("compactors wrote different checkpoints:\n%+v\n%+v", results[0].checkpoint, results[1].checkpoint)
	}
}

// failCheckpoints refuses to create checkpoint objects, as a compactor that
// crashes after writing the rest would leave the bucket.
type failCheckpoints struct{ blob.Store }

func (s failCheckpoints) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if strings.HasPrefix(key, checkpointPrefix) {
		return blob.Attributes{}, errors.New("compactor crashed")
	}
	return s.Store.Create(ctx, key, data)
}

// A checkpoint that never landed costs only replay: its orphaned blocks,
// shards and root change nothing, and a later compactor reuses them.
func TestCrashedCheckpointCostsOnlyReplay(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := manualSite(objects)
	writer := site.open(t, 0)
	writeWorld(t, writer, 0)
	snap := writer.served.Load().snap
	crashing := checkpointWriter{objects: failCheckpoints{objects}, worldID: testWorldID, workers: 4}
	if _, err := crashing.write(ctx, snap); err == nil {
		t.Fatal("checkpoint without its checkpoint object succeeded")
	}
	writeWorld(t, writer, 1)
	live := worldDigest(t, writer)
	cold := site.open(t, 0)
	if sequence := cold.served.Load().snap.Checkpoint.Sequence; sequence != 1 {
		t.Fatalf("cold start loaded checkpoint %d, want checkpoint zero", sequence)
	}
	if got := worldDigest(t, cold); !reflect.DeepEqual(live, got) {
		t.Errorf("replay past a crashed checkpoint differs:\nlive %+v\ncold %+v", live, got)
	}
	if _, err := (checkpointWriter{objects: objects, worldID: testWorldID, workers: 4}).write(ctx, snap); err != nil {
		t.Fatalf("checkpoint over the crashed one's objects: %v", err)
	}
}

// Retention that moves the first retained version inside one 256-version
// block while the tip is in the next rewrites the first block and keeps
// every retained version readable from the checkpoint.
func TestCheckpointPrunesAcrossHistoryBlocks(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := manualSite(objects)
	writer := site.open(t, 0)
	body := func(version int) []byte { return fmt.Appendf(nil, "# Doc\n\nversion %d\n", version) }
	for version := range 255 {
		_, err := writer.WriteVersion("/doc.md", version, body(version+1), nil)
		mustSucceed(t, err)
	}
	mustSucceed(t, writer.checkpoint(ctx))
	if blocks := writer.served.Load().snap.path("/doc.md").Base.History; len(blocks) != 1 || blocks[0].First != 1 || blocks[0].Last != 255 {
		t.Fatalf("blocks after 255 versions = %+v", blocks)
	}
	for version := 255; version < 258; version++ {
		_, err := writer.WriteVersion("/doc.md", version, body(version+1), map[string]string{"retention": "5"})
		mustSucceed(t, err)
	}
	mustSucceed(t, writer.checkpoint(ctx))
	want := []blockRef{{First: 254, Last: 256}, {First: 257, Last: 258}}
	blocks := writer.served.Load().snap.path("/doc.md").Base.History
	if len(blocks) != len(want) {
		t.Fatalf("blocks after pruning = %+v, want ranges %+v", blocks, want)
	}
	for index := range want {
		if blocks[index].First != want[index].First || blocks[index].Last != want[index].Last {
			t.Fatalf("blocks after pruning = %+v, want ranges %+v", blocks, want)
		}
	}

	cold := site.open(t, 0)
	versions, err := cold.Versions("/doc.md")
	mustSucceed(t, err)
	if len(versions) != 5 || versions[0].Version != 258 || versions[4].Version != 254 {
		t.Fatalf("versions = %+v, want 258 down to 254", versions)
	}
	for version := 254; version <= 258; version++ {
		document, err := cold.Get("/doc.md", version)
		if err != nil || !bytes.Equal(document.Content, body(version)) {
			t.Fatalf("v%d = %v, %v", version, document, err)
		}
	}
	if _, err := cold.Get("/doc.md", 253); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("pruned v253 = %v, want not found", err)
	}
	mustSucceed(t, cold.VerifyChain("/doc.md"))
}

// Archive state crosses a checkpoint both ways, and a document archived in
// the checkpoint unarchives and takes writes from a cold replica.
func TestCheckpointKeepsArchiveState(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := manualSite(objects)
	writer := site.open(t, 0)
	for _, path := range []string{"/docs/a.md", "/docs/b.md"} {
		_, err := writer.WriteVersion(path, 0, []byte("# "+path+"\n"), nil)
		mustSucceed(t, err)
	}
	_, _, err := writer.ArchiveResult("/docs/a.md", true)
	mustSucceed(t, err)
	mustSucceed(t, writer.checkpoint(ctx))

	cold := site.open(t, 0)
	document, err := cold.Get("/docs/a.md", 0)
	if err != nil || !document.Archived {
		t.Fatalf("archived document from the checkpoint = %+v, %v", document, err)
	}
	if entries, err := cold.ListEntries("/docs", false); err != nil || len(entries) != 1 || entries[0].Name != "b.md" {
		t.Fatalf("live listing = %+v, %v; want b.md alone", entries, err)
	}
	if _, err := cold.WriteVersion("/docs/a.md", 1, []byte("# again\n"), nil); !errors.Is(err, storefmt.ErrArchived) {
		t.Fatalf("write to an archived document = %v, want archived", err)
	}
	_, _, err = cold.ArchiveResult("/docs/a.md", false)
	mustSucceed(t, err)
	_, err = cold.WriteVersion("/docs/a.md", 1, []byte("# again\n"), nil)
	mustSucceed(t, err)
	_, _, err = cold.ArchiveResult("/docs/b.md", true)
	mustSucceed(t, err)
	mustSucceed(t, cold.checkpoint(ctx))
	mustSucceed(t, writer.poll(ctx))
	live := worldDigest(t, writer)
	if got := worldDigest(t, site.open(t, 0)); !reflect.DeepEqual(live, got) {
		t.Errorf("archive state across checkpoints differs:\nlive %+v\ncold %+v", live, got)
	}
}

// Export reads the same documents and versions whether the world comes from
// replay alone or from checkpoints.
func TestExportParityAcrossCheckpoints(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	writer := manualSite(objects).open(t, 0)
	for round := range 3 {
		writeWorld(t, writer, round)
	}
	exportAll := func() map[string]storefmt.StoredDocument {
		exported := make(map[string]storefmt.StoredDocument)
		mustSucceed(t, ExportDocs(ctx, objects, ExportOptions{WorldID: testWorldID}, func(path string, document storefmt.StoredDocument) error {
			exported[path] = document
			return nil
		}))
		return exported
	}
	replayed := exportAll()
	mustSucceed(t, writer.checkpoint(ctx))
	if checkpointed := exportAll(); !reflect.DeepEqual(replayed, checkpointed) {
		t.Errorf("export from the checkpoint differs from export by replay")
	}
}

// createCounts counts the objects created under each prefix.
type createCounts struct {
	blob.Store
	mu     sync.Mutex
	counts map[string]int
}

func (s *createCounts) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	attributes, err := s.Store.Create(ctx, key, data)
	if err == nil {
		s.mu.Lock()
		if s.counts == nil {
			s.counts = make(map[string]int)
		}
		s.counts[strings.SplitN(strings.TrimPrefix(key, objectPrefix), "/", 2)[0]]++
		s.mu.Unlock()
	}
	return attributes, err
}

func (s *createCounts) take() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := s.counts
	s.counts = nil
	return counts
}

// writeDocuments writes n new documents at once, so they batch.
func writeDocuments(t *testing.T, store *Store, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for index := range n {
		wg.Go(func() {
			if _, err := store.WriteVersion(fmt.Sprintf("/many/%04d.md", index), 0, fmt.Appendf(nil, "# %d\n", index), nil); err != nil {
				t.Errorf("write %d: %v", index, err)
			}
		})
	}
	wg.Wait()
}

// A checkpoint writes only what changed: one changed document costs its
// history block, its shard, the root and the checkpoint. A replica rebases
// on a peer's checkpoint by reading only the shards that changed.
func TestCheckpointsWriteAndReadOnlyChangedShards(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	created := &createCounts{Store: memory}
	observed := newObservedBlobStore(memory)
	writer := manualSite(created).open(t, 0)
	peer := (&bucketSite{objects: observed, trigger: manual, noHedge: true}).open(t, 0)
	writeDocuments(t, writer, 600)
	mustSucceed(t, writer.checkpoint(ctx))
	if bits := writer.layout().Bits; bits != 2 {
		t.Fatalf("600 documents in %d shard bits, want 2", bits)
	}
	mustSucceed(t, peer.poll(ctx))
	mustSucceed(t, peer.checkpoint(ctx))

	_, err := writer.WriteVersion("/many/0007.md", 1, []byte("# changed\n"), nil)
	mustSucceed(t, err)
	created.take()
	mustSucceed(t, writer.checkpoint(ctx))
	want := map[string]int{"history": 1, "index": 1, "roots": 1, "checkpoints": 1}
	if got := created.take(); !reflect.DeepEqual(got, want) {
		t.Errorf("checkpoint after one change created %v, want %v", got, want)
	}

	mustSucceed(t, peer.poll(ctx))
	observed.reset()
	mustSucceed(t, peer.checkpoint(ctx))
	if shards := countPrefix(observed.counts().gets, objectPrefix+"index/"); shards != 1 {
		t.Errorf("peer read %d shards to rebase, want the one that changed", shards)
	}
	state := peer.served.Load().snap.path("/many/0007.md")
	if state.Base == nil || state.Base.Current != 2 || len(state.Recent) != 0 {
		t.Errorf("peer's changed document after rebasing = base %+v recent %d, want the checkpoint's v2", state.Base, len(state.Recent))
	}
	if !reflect.DeepEqual(worldDigest(t, writer), worldDigest(t, peer)) {
		t.Error("peer differs from the writer after rebasing")
	}
}

func TestShardCount(t *testing.T) {
	for _, test := range []struct{ documents, bits int }{
		{0, 0}, {256, 0}, {257, 1}, {512, 1}, {513, 2}, {100_000, 9}, {1_000_000, 12}, {math.MaxInt32, maxShardBits},
	} {
		if got := shardBitsFor(test.documents); got != test.bits {
			t.Errorf("shard bits for %d documents = %d, want %d", test.documents, got, test.bits)
		}
	}
	hash := pathHash("/doc.md")
	for bits := range maxShardBits + 1 {
		if index := shardOf(hash, bits); index < 0 || index >= 1<<bits {
			t.Errorf("shard of %s at %d bits = %d", hash, bits, index)
		}
	}
	if shardLabel(0, 0) != "0" || shardLabel(5, 4) != "5" || shardLabel(0xabc, 12) != "abc" || shardLabel(0x1ff, 9) != "1ff" {
		t.Error("shard labels are not hex of their width")
	}
}

// A store compacts once its newest checkpoint lags by the trigger's slots or
// age; a read-only store never writes one but rebases on a peer's.
func TestCompactorRunsOnLag(t *testing.T) {
	quick := func(slots int, age time.Duration) *compactionTrigger {
		return &compactionTrigger{slots: slots, age: age, wait: func() time.Duration { return 0 }}
	}
	newest := func(t *testing.T, objects blob.Store) int64 {
		sequence, err := newestCheckpointSequence(context.Background(), objects, 0)
		mustSucceed(t, err)
		return sequence
	}

	t.Run("slots", func(t *testing.T) {
		objects := initializedMemory(t)
		writer := (&bucketSite{objects: objects, trigger: quick(4, time.Hour)}).open(t, 0)
		for version := range 3 {
			_, err := writer.WriteVersion("/doc.md", version, fmt.Appendf(nil, "v%d", version), nil)
			mustSucceed(t, err)
		}
		waitIdleCompactor(t, writer)
		if sequence := newest(t, objects); sequence != 1 {
			t.Fatalf("checkpoint %d written after 3 slots, want none", sequence)
		}
		_, err := writer.WriteVersion("/doc.md", 3, []byte("v3"), nil)
		mustSucceed(t, err)
		waitFor(t, "a checkpoint", func() bool { return writer.layout().Sequence == 5 })
	})

	t.Run("age", func(t *testing.T) {
		objects := initializedMemory(t)
		writer := (&bucketSite{objects: objects, trigger: quick(math.MaxInt, time.Millisecond)}).open(t, 0)
		_, err := writer.WriteVersion("/doc.md", 0, []byte("v0"), nil)
		mustSucceed(t, err)
		time.Sleep(5 * time.Millisecond)
		waitIdleCompactor(t, writer)
		if sequence := newest(t, objects); sequence != 1 {
			t.Fatalf("checkpoint %d written with no slot after the first, want none until the next", sequence)
		}
		_, err = writer.WriteVersion("/doc.md", 1, []byte("v1"), nil)
		mustSucceed(t, err)
		waitFor(t, "a checkpoint", func() bool { return newest(t, objects) == 3 })
	})

	t.Run("read-only rebases on a peer's", func(t *testing.T) {
		objects := initializedMemory(t)
		writer := manualSite(objects).open(t, 0)
		reader, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, ReadOnly: true, trigger: quick(1, time.Hour)})
		mustSucceed(t, err)
		closeAtEnd(t, reader)
		_, err = writer.WriteVersion("/doc.md", 0, []byte("v0"), nil)
		mustSucceed(t, err)
		mustSucceed(t, reader.poll(context.Background()))
		waitIdleCompactor(t, reader)
		if sequence := newest(t, objects); sequence != 1 {
			t.Fatalf("read-only store wrote checkpoint %d", sequence)
		}
		mustSucceed(t, writer.checkpoint(context.Background()))
		_, err = writer.WriteVersion("/doc.md", 1, []byte("v1"), nil)
		mustSucceed(t, err)
		mustSucceed(t, reader.poll(context.Background()))
		waitFor(t, "the reader to rebase", func() bool { return reader.layout().Sequence == 2 })
	})
}

// A checkpoint adopted while batches are in flight rebases each of them as it
// installs, so the served snapshot never falls back to holding the versions
// the checkpoint took.
func TestCheckpointDuringWritesRebasesEveryInstall(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := manualSite(objects)
	writer := site.open(t, 0)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for index := range 16 {
		wg.Go(func() {
			for round := 0; ; round++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := writer.WriteVersion(fmt.Sprintf("/w/%d.md", index), -1, fmt.Appendf(nil, "# %d.%d\n", index, round), nil); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		})
	}
	waitFor(t, "every document", func() bool { return writer.served.Load().snap.Paths.Len() == 16 })
	for range 5 {
		time.Sleep(5 * time.Millisecond)
		mustSucceed(t, writer.checkpoint(ctx))
	}
	close(stop)
	wg.Wait()
	waitIdle(t, writer)

	newest, err := newestCheckpointSequence(ctx, objects, 0)
	mustSucceed(t, err)
	snap := writer.served.Load().snap
	if snap.Checkpoint.Sequence != newest {
		t.Fatalf("served snapshot rests on checkpoint %d, want the newest, %d", snap.Checkpoint.Sequence, newest)
	}
	snap.Paths.Ascend(func(state *pathState) bool {
		if state.Base == nil {
			t.Errorf("%s has no checkpoint entry", state.Path)
			return true
		}
		for _, version := range state.Recent {
			if version.entry.Version <= state.Base.Current {
				t.Errorf("%s holds v%d, which its checkpoint entry has", state.Path, version.entry.Version)
			}
		}
		return true
	})
	if live, cold := worldDigest(t, writer), worldDigest(t, site.open(t, 0)); !reflect.DeepEqual(live, cold) {
		t.Error("cold replica differs from the writer")
	}
}

// listedCheckpoints counts the checkpoint names listings return.
type listedCheckpoints struct {
	blob.Store
	listed atomic.Int64
}

func (s *listedCheckpoints) List(ctx context.Context, prefix, startAfter, cursor string) (blob.ListResult, error) {
	result, err := s.Store.List(ctx, prefix, startAfter, cursor)
	if prefix == checkpointPrefix {
		s.listed.Add(int64(len(result.Objects)))
	}
	return result, err
}

// A compactor lists only the checkpoints after the one it rests on: until old
// ones are collected, a long-lived world holds one a minute.
func TestCompactorListsOnlyNewerCheckpoints(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	listing := &listedCheckpoints{Store: objects}
	writer := manualSite(listing).open(t, 0)
	peer := manualSite(objects).open(t, 0)
	for version := range 5 {
		_, err := writer.WriteVersion("/doc.md", version, fmt.Appendf(nil, "v%d", version), nil)
		mustSucceed(t, err)
		mustSucceed(t, writer.checkpoint(ctx))
	}
	listing.listed.Store(0)
	mustSucceed(t, writer.checkpoint(ctx))
	if listed := listing.listed.Load(); listed != 0 {
		t.Errorf("compactor resting on the newest checkpoint listed %d checkpoints, want none", listed)
	}
	mustSucceed(t, peer.poll(ctx))
	mustSucceed(t, peer.checkpoint(ctx))
	_, err := peer.WriteVersion("/doc.md", 5, []byte("v5"), nil)
	mustSucceed(t, err)
	mustSucceed(t, peer.checkpoint(ctx))
	listing.listed.Store(0)
	mustSucceed(t, writer.checkpoint(ctx))
	if listed := listing.listed.Load(); listed != 1 {
		t.Errorf("compactor listed %d checkpoints to find a peer's, want the one after its own", listed)
	}
	if writer.layout().Sequence != peer.layout().Sequence {
		t.Errorf("writer rests on checkpoint %d, peer wrote %d", writer.layout().Sequence, peer.layout().Sequence)
	}
}

// waitIdleCompactor waits until no compaction runs.
func waitIdleCompactor(t *testing.T, store *Store) {
	t.Helper()
	waitFor(t, "an idle compactor", func() bool {
		store.compaction.mu.Lock()
		defer store.compaction.mu.Unlock()
		return !store.compaction.running
	})
}

// A world whose newest checkpoint is schema 1 folds into the new format on
// its first checkpoint, reading each manifest once and reusing its blocks.
func TestCheckpointFoldsSchemaOne(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	commitReadDocuments(t, memory, []readDocumentSpec{
		newReadDocument("/docs/a.md", "# A v1\n", "# A v2\n"),
		newReadDocument("/docs/b.md", "# B\n"),
	})
	site := manualSite(memory)
	writer := site.open(t, 0)
	_, err := writer.WriteVersion("/docs/a.md", 2, []byte("# A v3\n"), nil)
	mustSucceed(t, err)
	mustSucceed(t, writer.checkpoint(ctx))
	if writer.layout().Legacy {
		t.Fatal("the checkpoint after a schema 1 one is schema 1")
	}
	live := worldDigest(t, writer)
	cold := site.open(t, 0)
	if got := worldDigest(t, cold); !reflect.DeepEqual(live, got) {
		t.Errorf("folded checkpoint differs:\nlive %+v\ncold %+v", live, got)
	}
	mustSucceed(t, cold.VerifyChain("/docs/a.md"))
}

// A folded root or shard that breaks the format's rules is refused.
func TestOpenRejectsFoldedInvariants(t *testing.T) {
	foldedWorld := func(t *testing.T) (*blob.Memory, foldedRoot, foldedShard) {
		objects := initializedMemory(t)
		writer := manualSite(objects).open(t, 0)
		_, err := writer.WriteVersion("/doc.md", 0, []byte("# Doc\n"), nil)
		mustSucceed(t, err)
		mustSucceed(t, writer.checkpoint(context.Background()))
		_, root := readFoldedRoot(t, objects)
		var shard foldedShard
		decodeObject(t, getObject(t, objects, root.Shards[0].Key).Data, &shard)
		return objects, root, shard
	}
	installShardOf := func(t *testing.T, objects blob.Store, root foldedRoot, shard foldedShard) {
		model, ref, err := immutableJSON(func(hash string) string { return shardKey(root.Shards[0].Shard, hash) }, shard)
		mustSucceed(t, err)
		createReadObject(t, objects, model)
		root.Shards[0] = shardRef{Shard: root.Shards[0].Shard, objectRef: ref}
		installRoot(t, objects, root)
	}
	tests := []struct {
		name   string
		mutate func(root *foldedRoot, shard *foldedShard)
	}{
		{name: "shard bits not the document count's", mutate: func(root *foldedRoot, _ *foldedShard) { root.ShardBits = 1 }},
		{name: "document count mismatch", mutate: func(root *foldedRoot, _ *foldedShard) { root.DocumentCount = 2 }},
		{name: "wrong world", mutate: func(root *foldedRoot, _ *foldedShard) { root.WorldID = otherWorldID }},
		{name: "wrong label", mutate: func(root *foldedRoot, _ *foldedShard) { root.Shards[0].Shard = "1" }},
		{name: "history short of current", mutate: func(_ *foldedRoot, shard *foldedShard) { shard.Entries[0].Current = 2 }},
		{name: "no history", mutate: func(_ *foldedRoot, shard *foldedShard) { shard.Entries[0].History = []blockRef{} }},
		{name: "shard bits differ from the root's", mutate: func(_ *foldedRoot, shard *foldedShard) { shard.ShardBits = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects, root, shard := foldedWorld(t)
			test.mutate(&root, &shard)
			installShardOf(t, objects, root, shard)
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, trigger: manual})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}
}

// Every write without retention adds a version to memory until a checkpoint
// takes it: with checkpoints, a long-running store's heap tracks its
// documents, not how many versions it has written.
func TestCheckpointedStoreHeapTracksLiveData(t *testing.T) {
	storeAfter := func(cycles int) int64 {
		objects := initializedMemory(t)
		owned := memtest.Owned(func() any {
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, trigger: manual})
			mustSucceed(t, err)
			for n := range cycles {
				var wg sync.WaitGroup
				for writer := range 48 {
					wg.Go(func() {
						body := fmt.Appendf(nil, "# Doc %d\n\nCycle %d at %d.\n", writer, n, time.Now().UnixNano())
						if _, err := store.WriteVersion(fmt.Sprintf("/d/%d.md", writer), -1, body, nil); err != nil {
							t.Errorf("write %d: %v", writer, err)
						}
					})
				}
				wg.Wait()
				mustSucceed(t, store.checkpoint(context.Background()))
			}
			waitIdle(t, store)
			mustSucceed(t, store.Close())
			return store
		})
		runtime.KeepAlive(objects)
		return owned
	}
	short, long := storeAfter(20), storeAfter(80)
	t.Logf("store heap after 20 cycles %d bytes, after 80 %d", short, long)
	if growth := long - short; growth > 256<<10 {
		t.Errorf("store heap grew %d bytes over 60 more cycles of 48 versions, want it to track 48 documents", growth)
	}
}
