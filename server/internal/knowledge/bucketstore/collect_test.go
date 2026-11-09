package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// aged is past the fence a manifest waits out before its blobs go.
const aged = slotRetention + time.Hour

// writeVersion writes the next version of path and returns its blob's key.
func writeVersion(t *testing.T, store *Store, path string, meta map[string]string) string {
	t.Helper()
	doc, err := store.WriteVersion(path, -1, fmt.Appendf(nil, "# Doc\n\nAt %d.\n", time.Now().UnixNano()), meta)
	mustSucceed(t, err)
	return blobKey(doc.ETag)
}

// writePruning writes n versions of path, the last keeping keep, and returns
// their blob keys.
func writePruning(t *testing.T, store *Store, path string, n, keep int) []string {
	t.Helper()
	keys := make([]string, 0, n)
	for version := 1; version <= n; version++ {
		var meta map[string]string
		if version == n {
			meta = map[string]string{"retention": strconv.Itoa(keep)}
		}
		keys = append(keys, writeVersion(t, store, path, meta))
	}
	return keys
}

// checkpointNext publishes one more document and checkpoints, dropping and
// collecting as a compactor's step does.
func checkpointNext(t *testing.T, store *Store) {
	t.Helper()
	publishSeq(t, store, store.servedSequence()+1)
	mustSucceed(t, store.checkpoint(context.Background()))
}

// gone counts the keys no longer in the bucket.
func gone(t *testing.T, objects blob.Store, keys []string) int {
	t.Helper()
	n := 0
	for _, key := range keys {
		if !exists(t, objects, key) {
			n++
		}
	}
	return n
}

// readKeptCheckpoints loads each checkpoint left without replaying the log
// and reads every version it retains, so a collected blob any of them names
// fails it.
func readKeptCheckpoints(t *testing.T, objects blob.Store) {
	t.Helper()
	ctx := context.Background()
	for _, sequence := range checkpointSequences(t, objects, 0) {
		checkpoint, err := readCheckpoint(ctx, objects, testWorldID, sequence)
		mustSucceed(t, err)
		snap, err := loadCheckpoint(ctx, objects, checkpoint, 4)
		mustSucceed(t, err)
		view := &readView{objects: objects, snapshot: snap}
		snap.Paths.Ascend(func(state *pathState) bool {
			history, err := view.history(ctx, state)
			if err != nil {
				t.Fatalf("checkpoint %d: history of %s: %v", sequence, state.Path, err)
			}
			for index := range history {
				if _, _, err := view.loadStored(ctx, state.Path, &history[index]); err != nil {
					t.Fatalf("checkpoint %d: %s v%d: %v", sequence, state.Path, history[index].entry.Version, err)
				}
			}
			return true
		})
	}
}

// Retention's pruned versions lose their blobs once the oldest kept
// checkpoint is past the prune and its manifest past the fence, wherever the
// versions were held: in the log only, in a checkpoint's blocks, or both.
func TestRetentionCollectsPrunedBlobs(t *testing.T) {
	tests := []struct {
		name          string
		before, after int // versions written before the first checkpoint, then after it
		keep          int // retention the last write declares
	}{
		{name: "pruned before any checkpoint", after: 4, keep: 1},
		{name: "pruned from whole checkpoint blocks", before: 3, after: 1, keep: 1},
		{name: "pruned within a checkpoint block", before: 3, after: 1, keep: 2},
		{name: "pruned from a checkpoint and the log", before: 3, after: 3, keep: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			objects := newClockedStore(t)
			objects.writtenAgo(aged)
			writer := manualSite(objects).open(t, 0)
			plain := []string{writeVersion(t, writer, "/plain.md", nil), writeVersion(t, writer, "/plain.md", nil)}
			var blobs []string
			if test.before > 0 {
				blobs = writePruning(t, writer, "/doc.md", test.before, test.before)
				mustSucceed(t, writer.checkpoint(ctx))
			}
			blobs = append(blobs, writePruning(t, writer, "/doc.md", test.after, test.keep)...)
			mustSucceed(t, writer.checkpoint(ctx))
			checkpointNext(t, writer)
			checkpointNext(t, writer)

			pruned := len(blobs) - test.keep
			for index, key := range blobs {
				if present := exists(t, objects, key); present == (index < pruned) {
					t.Errorf("v%d blob present = %t, want %t", index+1, present, index >= pruned)
				}
			}
			if n := gone(t, objects, plain); n != 0 {
				t.Errorf("%d blobs of a document without retention went", n)
			}
			for version := 1; version <= pruned; version++ {
				if _, err := writer.Get("/doc.md", version); !errors.Is(err, backend.ErrNotFound) || errors.Is(err, storefmt.ErrIntegrity) {
					t.Errorf("fetch pruned v%d: %v, want not found", version, err)
				}
			}
			if left := listKeys(t, objects, prunedPrefix); len(left) != 0 {
				t.Errorf("%d manifests left after their blobs went", len(left))
			}
			readEveryVersion(t, writer)
			readEveryVersion(t, manualSite(objects).open(t, 0))
			readKeptCheckpoints(t, objects)
		})
	}
}

// A kept checkpoint from before the prune still names the pruned version, so
// its blob stays however old the manifest is, until that checkpoint drops.
func TestCollectionWaitsForTheOldestKeptCheckpoint(t *testing.T) {
	ctx := context.Background()
	objects := newClockedStore(t)
	objects.writtenAgo(aged)
	writer := manualSite(objects).open(t, 0)
	first := writeVersion(t, writer, "/doc.md", nil)
	mustSucceed(t, writer.checkpoint(ctx))
	writeVersion(t, writer, "/doc.md", map[string]string{"retention": "1"})
	mustSucceed(t, writer.checkpoint(ctx))
	checkpointNext(t, writer)
	if !exists(t, objects, first) {
		t.Fatal("v1's blob went while a kept checkpoint retains v1")
	}
	readKeptCheckpoints(t, objects)
	checkpointNext(t, writer)
	if exists(t, objects, first) {
		t.Error("v1's blob stayed once no kept checkpoint retains v1")
	}
}

// A view pinned before the prune still reads the pruned version while its
// manifest is inside the fence, though every checkpoint holding it dropped.
func TestPinnedViewReadsAPrunedVersionInsideTheFence(t *testing.T) {
	ctx := context.Background()
	objects := newClockedStore(t)
	objects.writtenAgo(time.Hour)
	writer := manualSite(objects).open(t, 0)
	lagging := manualSite(objects).open(t, 0)
	first := writeVersion(t, writer, "/doc.md", nil)
	writeVersion(t, writer, "/doc.md", nil)
	pinned, err := lagging.openReadView()
	mustSucceed(t, err)
	t.Cleanup(func() { mustSucceed(t, pinned.Close()) })
	if _, err := pinned.Get("/doc.md", 2); err != nil {
		t.Fatalf("pin: %v", err)
	}
	writeVersion(t, writer, "/doc.md", map[string]string{"retention": "1"})
	mustSucceed(t, writer.checkpoint(ctx))
	checkpointNext(t, writer)
	checkpointNext(t, writer)

	plan, err := writer.planDrop(ctx)
	mustSucceed(t, err)
	manifests := listKeys(t, objects, prunedPrefix)
	if len(manifests) != 1 {
		t.Fatalf("%d manifests, want the prune's one", len(manifests))
	}
	if sequence, _, _ := prunedOfKey(manifests[0]); sequence > plan.oldestKept {
		t.Fatalf("manifest %d is past the oldest kept checkpoint %d; only the fence should hold it", sequence, plan.oldestKept)
	}
	if !exists(t, objects, first) {
		t.Fatal("v1's blob went inside the fence")
	}
	if _, err := pinned.Get("/doc.md", 1); err != nil {
		t.Errorf("pinned view reads pruned v1: %v", err)
	}
}

// Only a blob whose stored header names the manifest's path and version
// goes: a legacy blob names no path and may be shared, and a mismatch is not
// the pruned version.
func TestCollectionLeavesBlobsNamingAnotherVersion(t *testing.T) {
	ctx := context.Background()
	objects := newClockedStore(t)
	objects.writtenAgo(aged)
	writer := manualSite(objects).open(t, 0)
	stage := func(data []byte) objectRef {
		hash := hashHex(data)
		createReadObject(t, objects, modelObject{Key: blobKey(hash), Data: data})
		return objectRef{Key: blobKey(hash), Hash: hash}
	}
	serialized := func(path string) []byte {
		data, err := storefmt.SerializeVersion(&storefmt.VersionSpec{Path: path, Version: 1, Content: []byte("# " + path + "\n")})
		mustSucceed(t, err)
		return data
	}
	legacy := stage([]byte("---\nversion: 1\narchived: false\n---\n# Legacy\n"))
	doc := stage(serialized("/doc.md"))
	other := stage(serialized("/other.md"))
	match := stage(serialized("/match.md"))
	entries := []prunedEntry{
		{Path: "/doc.md", Version: 1, Blob: legacy},
		{Path: "/doc.md", Version: 2, Blob: doc},
		{Path: "/doc.md", Version: 3, Blob: other},
		{Path: "/match.md", Version: 1, Blob: match},
	}
	mustSucceed(t, testCheckpointWriter(objects).writePruned(ctx, genesisSequence, entries))
	plan, err := writer.planDrop(ctx)
	mustSucceed(t, err)
	writer.collectBlobs(ctx, plan)
	for _, ref := range []objectRef{legacy, doc, other} {
		if !exists(t, objects, ref.Key) {
			t.Errorf("blob %s went though its header does not name its entry", ref.Key)
		}
	}
	if exists(t, objects, match.Key) {
		t.Error("the blob whose header names its entry stayed")
	}
	if left := listKeys(t, objects, prunedPrefix); len(left) != 0 {
		t.Errorf("%d manifests left after every entry was done", len(left))
	}
}

// watchedKey is the one key a test's bucket wrapper acts on, set while
// stores run.
type watchedKey struct{ key atomic.Pointer[string] }

func (w *watchedKey) watch(key string) { w.key.Store(&key) }

func (w *watchedKey) is(key string) bool {
	watched := w.key.Load()
	return watched != nil && *watched == key
}

// blobFaults fails the next reads or deletes of the watched key.
type blobFaults struct {
	blob.Store
	watchedKey
	gets, deletes atomic.Int32
}

func (s *blobFaults) Get(ctx context.Context, key string) (blob.Object, error) {
	if s.is(key) && s.gets.Add(-1) >= 0 {
		return blob.Object{}, blob.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func (s *blobFaults) Delete(ctx context.Context, key string, generation blob.Generation) error {
	if s.is(key) && s.deletes.Add(-1) >= 0 {
		return blob.ErrUnavailable
	}
	return s.Store.Delete(ctx, key, generation)
}

// A failed read or delete of a pruned blob leaves the checkpoint written and
// the manifest in place, and the next step collects the blob.
func TestCollectionFaultsLeaveTheManifest(t *testing.T) {
	tests := []struct {
		name          string
		gets, deletes int32 // faults armed
	}{
		{name: "get", gets: 1},
		{name: "delete", deletes: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			clock := newClockedStore(t)
			clock.writtenAgo(aged)
			objects := &blobFaults{Store: clock}
			writer := manualSite(objects).open(t, 0)
			blobs := writePruning(t, writer, "/doc.md", 3, 1)
			mustSucceed(t, writer.checkpoint(ctx))
			checkpointNext(t, writer)
			objects.watch(blobs[0])
			objects.gets.Store(test.gets)
			objects.deletes.Store(test.deletes)
			checkpointNext(t, writer)
			if newest := newestCheckpointOf(t, objects); newest != writer.servedSequence() {
				t.Fatalf("newest checkpoint %d, want %d: a collection fault failed the step", newest, writer.servedSequence())
			}
			if !exists(t, objects, blobs[0]) || len(listKeys(t, objects, prunedPrefix)) == 0 {
				t.Fatal("the faulted blob or its manifest went")
			}
			checkpointNext(t, writer)
			if n := gone(t, objects, blobs[:2]); n != 2 {
				t.Errorf("%d of 2 pruned blobs went on the next step", n)
			}
			if left := listKeys(t, objects, prunedPrefix); len(left) != 0 {
				t.Errorf("%d manifests left after the retry", len(left))
			}
		})
	}
}

// A step collects at most its cap of blobs, so a large prune spreads over
// steps, each finishing whole manifests.
func TestCollectionSpreadsALargePruneOverSteps(t *testing.T) {
	ctx := context.Background()
	objects := newClockedStore(t)
	objects.writtenAgo(aged)
	trigger := *manual
	trigger.collect = 2
	writer := (&bucketSite{objects: objects, trigger: &trigger}).open(t, 0)
	pruned := writePruning(t, writer, "/doc.md", 7, 1)[:6]
	mustSucceed(t, writer.checkpoint(ctx))
	checkpointNext(t, writer)
	if n := len(listKeys(t, objects, prunedPrefix)); n != 3 {
		t.Fatalf("%d manifests for 6 pruned versions in parts of 2, want 3", n)
	}
	for step := 1; step <= 3; step++ {
		checkpointNext(t, writer)
		if n := gone(t, objects, pruned); n != 2*step {
			t.Errorf("step %d: %d blobs gone, want %d", step, n, 2*step)
		}
	}
	if left := listKeys(t, objects, prunedPrefix); len(left) != 0 {
		t.Errorf("%d manifests left", len(left))
	}
}

// Two compactors collecting the same manifests at once both finish cleanly:
// a delete the other won is done, never an error.
func TestRacingCollectorsAgree(t *testing.T) {
	ctx := context.Background()
	objects := newClockedStore(t)
	objects.writtenAgo(aged)
	var logs bytes.Buffer
	warnings := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	open := func() *Store {
		return openSectionStore(t, objects, func(options *Options) { options.Logger = warnings })
	}
	writer, peer := open(), open()
	blobs := writePruning(t, writer, "/doc.md", 40, 1)
	for range 3 {
		checkpointOnly(t, writer)
		publishSeq(t, writer, writer.servedSequence()+1)
	}
	checkpointOnly(t, writer)
	mustSucceed(t, peer.poll(ctx))
	var wg sync.WaitGroup
	for _, store := range []*Store{writer, peer} {
		wg.Go(func() {
			plan, err := store.planDrop(ctx)
			if err != nil {
				t.Errorf("plan: %v", err)
				return
			}
			store.collectBlobs(ctx, plan)
		})
	}
	wg.Wait()
	if n := gone(t, objects, blobs); n != 39 {
		t.Errorf("%d of 39 pruned blobs went", n)
	}
	if left := listKeys(t, objects, prunedPrefix); len(left) != 0 {
		t.Errorf("%d manifests left", len(left))
	}
	if logs.Len() > 0 {
		t.Errorf("racing collectors warned:\n%s", logs.String())
	}
	readEveryVersion(t, writer)
}

// recreateOnGet writes the watched key again, a new generation, right after
// it is first read.
type recreateOnGet struct {
	blob.Store
	watchedKey
	once sync.Once
}

func (s *recreateOnGet) Get(ctx context.Context, key string) (blob.Object, error) {
	object, err := s.Store.Get(ctx, key)
	if err == nil && s.is(key) {
		s.once.Do(func() {
			_, err = s.Replace(ctx, key, object.Attributes.Generation, object.Data)
		})
	}
	return object, err
}

// A blob written again after the collector read it is a new generation the
// delete is not conditioned on, so it stays.
func TestCollectionLeavesABlobRecreatedAfterItsRead(t *testing.T) {
	ctx := context.Background()
	clock := newClockedStore(t)
	clock.writtenAgo(aged)
	objects := &recreateOnGet{Store: clock}
	writer := manualSite(objects).open(t, 0)
	blobs := writePruning(t, writer, "/doc.md", 3, 1)
	objects.watch(blobs[0])
	mustSucceed(t, writer.checkpoint(ctx))
	checkpointNext(t, writer)
	checkpointNext(t, writer)
	if !exists(t, objects, blobs[0]) {
		t.Error("the blob recreated after its read went")
	}
	if exists(t, objects, blobs[1]) {
		t.Error("the other pruned blob stayed")
	}
}

// A hot document under keep=1 holds the blobs of the versions it pruned in
// memory only until the next checkpoint rebases it, which releases them.
func TestPrunedVersionsReleaseAtRebase(t *testing.T) {
	ctx := context.Background()
	objects := &storedBytes{Store: initializedMemoryOf(t, 4<<20)}
	store := openSectionStore(t, objects, nil)
	const writes = 2000
	for write := range writes {
		_, err := store.WriteVersion("/hot.md", -1, fmt.Appendf(nil, "# Hot\n\n%d\n", write), map[string]string{"retention": "1"})
		mustSucceed(t, err)
	}
	held := func() int { return prunedHeld(store.served.Load().snap.path("/hot.md")) }
	if n := held(); n != writes-1 {
		t.Fatalf("%d pruned versions held before the checkpoint, want the %d pruned since", n, writes-1)
	}
	waitIdle(t, store)
	bytesBefore, objectsBefore := objects.bytes.Load(), objects.objects.Load()
	released := memtest.Retained(func() { mustSucceed(t, store.checkpoint(ctx)) })
	runtime.KeepAlive(store)
	if n := held(); n != 0 {
		t.Fatalf("%d pruned versions held after the checkpoint, want none", n)
	}
	// The checkpoint's objects, its manifest above all, stay in the bucket.
	bucket := objects.bytes.Load() - bytesBefore + 512*(objects.objects.Load()-objectsBefore)
	freed := bucket - released
	t.Logf("checkpoint freed %d bytes outside the bucket, %d per pruned version", freed, freed/writes)
	// A held version costs about 110 bytes, its node and hash; the in-memory
	// bucket keeps its key.
	if freed < writes*64 {
		t.Errorf("checkpoint freed %d bytes outside the bucket, want the %d held versions' (over %d)", freed, writes, writes*64)
	}
}

// prunedHeld counts the pruned versions a document holds for its next
// checkpoint.
func prunedHeld(state *pathState) int {
	n := 0
	for node := state.Pruned; node != nil; node = node.next {
		n++
	}
	return n
}
