package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// A content object the bucket rejects (over its size limit) fails the
// publish: no version is acknowledged over a body that was never stored.
func TestRejectedContentFailsThePublish(t *testing.T) {
	objects := initializedMemoryOf(t, 4096)
	store := manualSite(objects).open(t, 0)
	_, err := store.WriteVersion("/large.md", 0, []byte(strings.Repeat("x", 8192)), nil)
	if !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("publish over the object limit = %v, want ErrPrecondition", err)
	}
	if _, err := store.Get("/large.md", 0); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Get after the rejected publish = %v, want not found", err)
	}
	_, err = store.WriteVersion("/small.md", 0, []byte("# Small\n"), nil)
	mustSucceed(t, err)
}

// A body-sized heading does not inflate the catalog entry: documents whose
// first heading runs to the body limit still fold into one shard.
func TestLongHeadingsFoldWithinTheShardBudget(t *testing.T) {
	objects := initializedMemoryOf(t, MaxObjectBytes)
	store := manualSite(objects).open(t, 0)
	body := []byte("# " + strings.Repeat("x", 900000) + "\n")
	for i := range 5 {
		_, err := store.WriteVersion(fmt.Sprintf("/doc%d.md", i), 0, body, nil)
		mustSucceed(t, err)
	}
	mustSucceed(t, store.checkpoint(context.Background()))
	_, root := readFoldedRoot(t, objects)
	if root.ShardBits != 0 {
		t.Fatalf("shard bits = %d, want 0 for 5 documents", root.ShardBits)
	}
	if size := getObject(t, objects, root.Shards[0].Key).Attributes.Size; size > maxShardBytes {
		t.Fatalf("shard is %d bytes, over the %d budget", size, maxShardBytes)
	}
}

// wideMetadata fills the metadata cap, so under a small shard budget a few
// documents outgrow a shard's bytes long before its count.
var wideMetadata = map[string]string{"description": strings.Repeat("d", 900)}

// Shards that outgrow the byte budget double the shard count past the count
// rule; the layout loads, and the next checkpoint keeps it.
func TestOversizedShardsWidenTheLayout(t *testing.T) {
	objects := initializedMemory(t)
	const documents = 8
	store := (&bucketSite{objects: objects, trigger: manual, shardBudget: 4096}).open(t, 0)
	for i := range documents {
		_, err := store.WriteVersion(fmt.Sprintf("/doc%d.md", i), 0, []byte("# Doc\n"), wideMetadata)
		mustSucceed(t, err)
	}
	mustSucceed(t, store.checkpoint(context.Background()))
	_, root := readFoldedRoot(t, objects)
	if root.ShardBits <= shardBitsFor(documents) {
		t.Fatalf("shard bits = %d, want more than the count rule's %d", root.ShardBits, shardBitsFor(documents))
	}
	for _, ref := range root.Shards {
		if size := getObject(t, objects, ref.Key).Attributes.Size; size > 4096 {
			t.Fatalf("shard %s is %d bytes, over the 4096 budget", ref.Shard, size)
		}
	}
	widened := root.ShardBits

	_, err := store.WriteVersion("/doc0.md", -1, []byte("# Doc\n\nchanged\n"), wideMetadata)
	mustSucceed(t, err)
	before := countObjects(t, objects)
	mustSucceed(t, store.checkpoint(context.Background()))
	_, root = readFoldedRoot(t, objects)
	if root.ShardBits != widened {
		t.Fatalf("shard bits after a one-document change = %d, want the widened %d kept", root.ShardBits, widened)
	}
	// One shard, the root, the checkpoint and the shard's segment: not a refold.
	if added := countObjects(t, objects) - before; added > 4 {
		t.Fatalf("a one-document checkpoint added %d objects, want at most 4", added)
	}

	peer := manualSite(objects).open(t, 0)
	document, err := peer.Get("/doc0.md", 0)
	mustSucceed(t, err)
	if !strings.Contains(string(document.Content), "changed") {
		t.Fatalf("peer read %q, want the changed body", document.Content)
	}
}

// One document whose entry alone exceeds the budget fails the checkpoint
// naming the shard, instead of widening to no effect.
func TestOneOversizedEntryFailsTheCheckpoint(t *testing.T) {
	objects := initializedMemory(t)
	store := (&bucketSite{objects: objects, trigger: manual, shardBudget: 512}).open(t, 0)
	_, err := store.WriteVersion("/wide.md", 0, []byte("# Wide\n"), wideMetadata)
	mustSucceed(t, err)
	err = store.checkpoint(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("checkpoint of one oversized entry = %v, want an error naming the shard", err)
	}
}
