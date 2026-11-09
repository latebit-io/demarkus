package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// worldView is what readers see of a store: every document with its history,
// and its catalog and body lookups.
type worldView struct {
	world   digest
	catalog []catalog.Result
	body    []string
}

func viewOf(t *testing.T, store *Store) worldView {
	t.Helper()
	results, err := store.Lookup("checkpoint", catalog.Options{})
	mustSucceed(t, err)
	if len(results) == 0 {
		t.Fatal("catalog lookup found nothing to compare")
	}
	return worldView{world: worldDigest(t, store), catalog: results, body: settledRows(t, store, "body")}
}

func assertView(t *testing.T, store *Store, want worldView) {
	t.Helper()
	if got := viewOf(t, store); !reflect.DeepEqual(got, want) {
		t.Errorf("world differs from its writer's:\ngot  %+v\nwant %+v", got, want)
	}
}

// fallbackWorld is writeFallbackWorld on a new bucket, which it returns.
func fallbackWorld(t *testing.T) (objects *blob.Memory, newest, older int64, want worldView) {
	t.Helper()
	objects = initializedMemory(t)
	newest, older, want = writeFallbackWorld(t, objects)
	return objects, newest, older, want
}

// writeFallbackWorld writes three rounds of every kind of change,
// checkpointing after the first two, and returns the newest checkpoint, the
// one before it, and the world as its writer sees it.
func writeFallbackWorld(t *testing.T, objects blob.Store) (newest, older int64, want worldView) {
	t.Helper()
	writer := manualSite(objects).open(t, 0)
	for round := range 3 {
		writeWorld(t, writer, round)
		if round < 2 {
			mustSucceed(t, writer.checkpoint(context.Background()))
		}
	}
	sequences := checkpointSequences(t, objects, 0)
	if len(sequences) != 3 {
		t.Fatalf("checkpoints %v, want zero and two more", sequences)
	}
	return sequences[2], sequences[1], viewOf(t, writer)
}

// damageEveryCheckpoint applies damage to every checkpoint's key.
func damageEveryCheckpoint(t *testing.T, objects blob.Store, damage func(*testing.T, blob.Store, string)) {
	t.Helper()
	for _, sequence := range checkpointSequences(t, objects, 0) {
		damage(t, objects, checkpointKey(sequence))
	}
}

// rootAt reads the checkpoint at sequence and its root.
func rootAt(t *testing.T, objects blob.Store, sequence int64) (checkpointObject, foldedRoot) {
	t.Helper()
	checkpoint, err := readCheckpoint(context.Background(), objects, testWorldID, sequence)
	mustSucceed(t, err)
	var root foldedRoot
	decodeObject(t, getObject(t, objects, checkpoint.Root.Key).Data, &root)
	return checkpoint, root
}

// deleteSlots deletes the slots whose first sequence is from through last.
func deleteSlots(t *testing.T, objects blob.Store, from, last int64) {
	t.Helper()
	for page, err := range sequencedPages(context.Background(), objects, logPrefix, from-1) {
		mustSucceed(t, err)
		for index := range page {
			if page[index].sequence > last {
				return
			}
			deleteObject(t, objects, page[index].Key)
		}
	}
}

// A checkpoint that does not load costs only replay: Open falls back to the
// one before it and serves the same world.
func TestOpenFallsBackPastABrokenCheckpoint(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, objects blob.Store, newest int64)
	}{
		{name: "corrupt checkpoint", damage: func(t *testing.T, objects blob.Store, newest int64) {
			corruptObject(t, objects, checkpointKey(newest))
		}},
		{name: "corrupt root", damage: func(t *testing.T, objects blob.Store, newest int64) {
			checkpoint, _ := rootAt(t, objects, newest)
			corruptObject(t, objects, checkpoint.Root.Key)
		}},
		{name: "missing root", damage: func(t *testing.T, objects blob.Store, newest int64) {
			checkpoint, _ := rootAt(t, objects, newest)
			deleteObject(t, objects, checkpoint.Root.Key)
		}},
		{name: "missing shard", damage: func(t *testing.T, objects blob.Store, newest int64) {
			_, root := rootAt(t, objects, newest)
			deleteObject(t, objects, root.Shards[0].Key)
		}},
		{name: "corrupt shard", damage: func(t *testing.T, objects blob.Store, newest int64) {
			_, root := rootAt(t, objects, newest)
			corruptObject(t, objects, root.Shards[0].Key)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects, newest, older, want := fallbackWorld(t)
			test.damage(t, objects, newest)
			store := manualSite(objects).open(t, 0)
			if got := store.served.Load().snap.Checkpoint.Sequence; got != older {
				t.Fatalf("loaded checkpoint %d, want %d before the broken %d", got, older, newest)
			}
			assertView(t, store, want)
		})
	}
}

// With no checkpoint that loads, a log that still starts at its first slot
// replays from genesis, the empty world checkpoint zero holds.
func TestOpenReplaysFromGenesisWhenNoCheckpointLoads(t *testing.T) {
	tests := []struct {
		name   string
		damage func(*testing.T, blob.Store, string)
	}{{name: "corrupt", damage: corruptObject}, {name: "deleted", damage: deleteObject}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects, _, _, want := fallbackWorld(t)
			damageEveryCheckpoint(t, objects, test.damage)
			store := manualSite(objects).open(t, 0)
			if got := store.served.Load().snap.Checkpoint.Sequence; got != genesisSequence {
				t.Fatalf("loaded checkpoint %d, want genesis", got)
			}
			assertView(t, store, want)
		})
	}
}

// The genesis a replay starts from without a checkpoint is checkpoint zero.
func TestGenesisSnapshotIsCheckpointZero(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	checkpoint, err := readCheckpoint(ctx, objects, testWorldID, genesisSequence)
	mustSucceed(t, err)
	loaded, err := loadCheckpoint(ctx, objects, checkpoint, 1)
	mustSucceed(t, err)
	built, err := genesisSnapshot()
	mustSucceed(t, err)
	if !reflect.DeepEqual(snapshotDigest(built), snapshotDigest(loaded)) || !reflect.DeepEqual(built.Checkpoint, loaded.Checkpoint) {
		t.Errorf("genesis %+v on %+v, checkpoint zero %+v on %+v", snapshotDigest(built), built.Checkpoint, snapshotDigest(loaded), loaded.Checkpoint)
	}
}

// Replay never skips a slot: a world whose first slots were collected, as
// releases before the log was kept did, or that lacks a slot after the
// checkpoint it falls back to, up to the newest listed one, fails to open.
func TestOpenFallbackNeedsEverySlotAfterIt(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, objects blob.Store, newest, older int64)
	}{
		{name: "no checkpoint loads, first slots collected", damage: func(t *testing.T, objects blob.Store, _, older int64) {
			damageEveryCheckpoint(t, objects, corruptObject)
			deleteSlots(t, objects, 2, older)
		}},
		{name: "no checkpoint loads, one slot missing", damage: func(t *testing.T, objects blob.Store, _, older int64) {
			damageEveryCheckpoint(t, objects, corruptObject)
			deleteSlots(t, objects, older, older)
		}},
		{name: "newest broken, a slot after the older missing", damage: func(t *testing.T, objects blob.Store, newest, older int64) {
			corruptObject(t, objects, checkpointKey(newest))
			deleteSlots(t, objects, older+1, older+1)
		}},
		{name: "newest broken, a slot after the older corrupt", damage: func(t *testing.T, objects blob.Store, newest, older int64) {
			corruptObject(t, objects, checkpointKey(newest))
			corruptObject(t, objects, slotKey(older+1))
		}},
		{name: "newest broken, every slot after the older missing", damage: func(t *testing.T, objects blob.Store, newest, older int64) {
			corruptObject(t, objects, checkpointKey(newest))
			deleteSlots(t, objects, older+1, math.MaxInt64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects, newest, older, _ := fallbackWorld(t)
			test.damage(t, objects, newest, older)
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, trigger: manual})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}
}

// failingGets fails every read of one key with err.
type failingGets struct {
	blob.Store
	key string
	err error
}

func (s *failingGets) Get(ctx context.Context, key string) (blob.Object, error) {
	if key == s.key {
		return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: s.err}
	}
	return s.Store.Get(ctx, key)
}

// A transient failure reading the newest checkpoint or its shard is no
// reason to fall back: Open returns it and reads no older checkpoint.
func TestOpenDoesNotFallBackOnATransientFailure(t *testing.T) {
	for _, category := range []error{blob.ErrUnavailable, blob.ErrThrottled, context.DeadlineExceeded} {
		for _, object := range []string{"checkpoint", "shard"} {
			t.Run(category.Error()+" "+object, func(t *testing.T) {
				objects, newest, older, _ := fallbackWorld(t)
				key := checkpointKey(newest)
				if object == "shard" {
					_, root := rootAt(t, objects, newest)
					key = root.Shards[0].Key
				}
				observed := newObservedBlobStore(&failingGets{Store: objects, key: key, err: category})
				store, err := Open(context.Background(), observed, Options{Logger: discardLogger, WorldID: testWorldID, noHedge: true, trigger: manual})
				if store != nil || !errors.Is(err, category) || errors.Is(err, blob.ErrIntegrity) {
					t.Fatalf("Open() = (%v, %v), want nil and %v alone", store, err, category)
				}
				if n := observed.counts().gets[checkpointKey(older)]; n != 0 {
					t.Errorf("older checkpoint read %d times, want no fallback", n)
				}
			})
		}
	}
}

// A compactor skips a newer checkpoint that does not load rather than failing
// every run, and writes its next one past it, never over it.
func TestCompactorSkipsABrokenNewerCheckpoint(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	writer := manualSite(objects).open(t, 0)
	// A step bound of a nanosecond: the peer writes without deferring to the
	// writer's fresh checkpoint.
	peer := (&bucketSite{objects: objects, trigger: stepping(0, time.Nanosecond)}).open(t, 0)
	writeWorld(t, writer, 0)
	mustSucceed(t, writer.checkpoint(ctx))
	own := writer.layout().Sequence
	writeWorld(t, writer, 1)
	mustSucceed(t, peer.poll(ctx))
	mustSucceed(t, peer.checkpoint(ctx)) // adopts the writer's
	mustSucceed(t, peer.checkpoint(ctx))
	broken := peer.layout().Sequence
	if broken != writer.servedSequence() {
		t.Fatalf("peer checkpointed %d, want the tip %d", broken, writer.servedSequence())
	}
	corruptObject(t, objects, checkpointKey(broken))
	before := getObject(t, objects, checkpointKey(broken)).Data

	mustSucceed(t, writer.checkpoint(ctx))
	if got := writer.layout().Sequence; got != own {
		t.Fatalf("writer rests on checkpoint %d, want its own %d: nothing lies past the broken one", got, own)
	}
	writeWorld(t, writer, 2)
	mustSucceed(t, writer.checkpoint(ctx))
	if got := writer.layout().Sequence; got != writer.servedSequence() || got <= broken {
		t.Fatalf("writer checkpointed %d, want the tip %d past the broken %d", got, writer.servedSequence(), broken)
	}
	if !bytes.Equal(getObject(t, objects, checkpointKey(broken)).Data, before) {
		t.Error("the broken checkpoint was rewritten")
	}
	cold := manualSite(objects).open(t, 0)
	if got := cold.served.Load().snap.Checkpoint.Sequence; got != writer.layout().Sequence {
		t.Fatalf("cold start loaded checkpoint %d, want %d", got, writer.layout().Sequence)
	}
	assertView(t, cold, viewOf(t, writer))
}

// A bounded step that would end on a broken checkpoint's sequence goes one
// slot further: create-if-absent would refuse its bytes there on every run.
func TestCompactorStepsPastABrokenCheckpointsSequence(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := &bucketSite{objects: objects, trigger: stepping(16, 0), noHedge: true}
	writer, peer := site.open(t, 0), site.open(t, 0)
	writeDocuments(t, writer, 200)
	mustSucceed(t, peer.poll(ctx))
	more, err := peer.step(ctx)
	mustSucceed(t, err)
	broken := peer.layout().Sequence
	if !more || broken <= 1 || broken >= writer.servedSequence() {
		t.Fatalf("peer's step checkpointed %d of %d, want a bounded step", broken, writer.servedSequence())
	}
	corruptObject(t, objects, checkpointKey(broken))
	before := getObject(t, objects, checkpointKey(broken)).Data

	mustSucceed(t, writer.checkpoint(ctx))
	if got := writer.layout().Sequence; got != writer.servedSequence() {
		t.Fatalf("writer checkpointed %d, want the tip %d", got, writer.servedSequence())
	}
	if !bytes.Equal(getObject(t, objects, checkpointKey(broken)).Data, before) {
		t.Error("the broken checkpoint was rewritten")
	}
	live := worldDigest(t, writer)
	if got := worldDigest(t, site.open(t, 0)); !reflect.DeepEqual(got, live) {
		t.Errorf("cold start differs from the writer:\ngot  %+v\nwant %+v", got, live)
	}
}

// A stale or diverged replica reloading past a checkpoint that does not load
// falls back to the one before it instead of failing every read.
func TestReloadFallsBackPastABrokenCheckpoint(t *testing.T) {
	for _, reason := range []string{"stale", "diverged"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			objects := initializedMemory(t)
			replica := manualSite(objects).open(t, 0)
			newest, older, want := writeFallbackWorld(t, objects)
			corruptObject(t, objects, checkpointKey(newest))
			if reason == "stale" {
				unconfirmed(replica)
			} else {
				replica.diverged.Store(true)
			}

			if _, err := replica.refresh(ctx); err != nil {
				t.Fatalf("refresh past the broken checkpoint: %v", err)
			}
			if got := replica.served.Load().snap.Checkpoint.Sequence; got != older {
				t.Fatalf("reloaded on checkpoint %d, want %d before the broken %d", got, older, newest)
			}
			if replica.diverged.Load() {
				t.Error("still diverged after the reload")
			}
			assertView(t, replica, want)
		})
	}
}

// A stale replica none of whose newer checkpoints loads reloads from an older
// one and replays to the newest; with a slot it needs collected, it fails
// rather than take its own sequence for the tip.
func TestStaleReloadPastBrokenNewerCheckpoints(t *testing.T) {
	for _, collected := range []bool{false, true} {
		t.Run(fmt.Sprintf("next slot collected %t", collected), func(t *testing.T) {
			ctx := context.Background()
			objects := initializedMemory(t)
			writer := manualSite(objects).open(t, 0)
			writeWorld(t, writer, 0)
			mustSucceed(t, writer.checkpoint(ctx))
			replica := manualSite(objects).open(t, 0)
			served := replica.servedSequence()
			writeWorld(t, writer, 1)
			mustSucceed(t, writer.checkpoint(ctx))
			corruptObject(t, objects, checkpointKey(newestCheckpointOf(t, objects)))
			if collected {
				deleteSlots(t, objects, served+1, served+1)
			}
			unconfirmed(replica)

			_, err := replica.refresh(ctx)
			if collected {
				if !errors.Is(err, blob.ErrIntegrity) {
					t.Fatalf("refresh past a collected slot = %v, want ErrIntegrity", err)
				}
				return
			}
			mustSucceed(t, err)
			assertView(t, replica, viewOf(t, writer))
		})
	}
}
