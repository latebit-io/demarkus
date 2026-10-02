package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

func TestInitializeGenesis(t *testing.T) {
	objects := newTestMemory(t)
	if err := initialize(context.Background(), objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	t.Run("object graph", func(t *testing.T) {
		listed, err := objects.List(context.Background(), "", "", "")
		if err != nil {
			t.Fatalf("list objects: %v", err)
		}
		if len(listed.Objects) != 4 || listed.NextCursor != "" {
			t.Fatalf("object count = %d cursor %q, want 4 and empty", len(listed.Objects), listed.NextCursor)
		}
		var marker markerObject
		decodeObject(t, getObject(t, objects, markerKey).Data, &marker)
		if marker != (markerObject{Schema: logSchema, WorldID: testWorldID}) {
			t.Fatalf("marker = %+v", marker)
		}
		var checkpoint checkpointObject
		decodeObject(t, getObject(t, objects, checkpointKey(1)).Data, &checkpoint)
		if checkpoint.Schema != logSchema || checkpoint.WorldID != testWorldID || checkpoint.Sequence != 1 || checkpoint.Tip != "" {
			t.Fatalf("checkpoint zero = %+v", checkpoint)
		}

		rootValue := getObject(t, objects, checkpoint.Root.Key)
		if hashHex(rootValue.Data) != checkpoint.Root.Hash || checkpoint.Root.Key != rootKey(checkpoint.Root.Hash) {
			t.Fatalf("root identity does not match checkpoint zero: %+v", checkpoint.Root)
		}
		var root foldedRoot
		decodeObject(t, rootValue.Data, &root)
		if root.Schema != foldedSchema || root.DocumentCount != 0 || root.ShardBits != 0 || len(root.Shards) != 1 {
			t.Fatalf("root = %+v, want one folded shard and no documents", root)
		}
		ref := root.Shards[0]
		if ref.Shard != "0" || ref.Key != shardKey("0", ref.Hash) {
			t.Fatalf("shard ref = %+v", ref)
		}
		value := getObject(t, objects, ref.Key)
		if hashHex(value.Data) != ref.Hash {
			t.Fatal("shard hash mismatch")
		}
		var shard foldedShard
		decodeObject(t, value.Data, &shard)
		if shard.Entries == nil || len(shard.Entries) != 0 || !bytes.Contains(value.Data, []byte(`"entries":[]`)) {
			t.Fatalf("shard entries = %#v bytes=%s", shard.Entries, value.Data)
		}
	})

	t.Run("open defaults", func(t *testing.T) {
		store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if store.requestTimeout != defaultRequestTimeout || store.shardWorkers != defaultShardWorkers {
			t.Errorf("defaults timeout=%v workers=%d", store.requestTimeout, store.shardWorkers)
		}
		loaded := store.served.Load().snap
		if loaded == nil || loaded.Sequence != 1 || loaded.Paths.Len() != 0 || loaded.Hashes.Len() != 0 || loaded.Children.Len() != 0 {
			t.Fatalf("empty snapshot = %+v", loaded)
		}
		if !loaded.isDirectory("/") {
			t.Error("an empty world has no root directory")
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		before := getObject(t, objects, markerKey).Attributes.Generation
		if err := initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("reinitialize: %v", err)
		}
		after := getObject(t, objects, markerKey).Attributes.Generation
		if after != before {
			t.Errorf("head generation changed from %d to %d", before, after)
		}
		listed, err := objects.List(context.Background(), "", "", "")
		if err != nil {
			t.Fatalf("list after reinitialize: %v", err)
		}
		if len(listed.Objects) != 4 {
			t.Errorf("object count after reinitialize = %d", len(listed.Objects))
		}
	})
}

func TestValidatePolicyOnBucketStore(t *testing.T) {
	objects := initializedMemory(t)
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := writepolicy.Validate(context.Background(), store); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Fatalf("Validate without policy = %v, want ErrInvalidPolicy", err)
	}
	seedPolicy(t, store, "strictness: block\n", 0)
	if err := writepolicy.Validate(context.Background(), store); err != nil {
		t.Fatalf("Validate with policy: %v", err)
	}
}

func TestSeparateBucketsAreIndependent(t *testing.T) {
	const otherWorldID = "7d4f3f8a-87f0-4bf5-932a-e4d1db28d235"
	open := func(worldID string) *Store {
		objects := newTestMemory(t)
		if err := initialize(context.Background(), objects, worldID); err != nil {
			t.Fatalf("initialize %s: %v", worldID, err)
		}
		store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: worldID})
		if err != nil {
			t.Fatalf("open %s: %v", worldID, err)
		}
		return store
	}
	left := open(testWorldID)
	right := open(otherWorldID)
	body := []byte("shared body")
	for _, store := range []*Store{left, right} {
		if _, err := store.WriteVersion("/same", 0, body, nil); err != nil {
			t.Fatalf("write shared document: %v", err)
		}
	}
	if _, err := left.WriteVersion("/same", 1, []byte("left v2"), nil); err != nil {
		t.Fatalf("write left v2: %v", err)
	}
	if _, _, err := left.ArchiveResult("/same", true); err != nil {
		t.Fatalf("archive left: %v", err)
	}

	leftDocument, err := left.Get("/same", 0)
	if err != nil || leftDocument.Version != 2 || !leftDocument.Archived {
		t.Fatalf("left document = (%+v, %v), want archived v2", leftDocument, err)
	}
	rightDocument, err := right.Get("/same", 0)
	if err != nil || rightDocument.Version != 1 || rightDocument.Archived || !bytes.Equal(rightDocument.Content, body) {
		t.Fatalf("right document = (%+v, %v), want active v1", rightDocument, err)
	}
	if _, err := left.LookupHash(storefmt.ContentHash(body)); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("left shared hash error = %v, want ErrNotExist", err)
	}
	if path, err := right.LookupHash(storefmt.ContentHash(body)); err != nil || path != "/same" {
		t.Fatalf("right shared hash = (%q, %v), want /same", path, err)
	}
}

func TestInitializeReconciliation(t *testing.T) {
	t.Run("ambiguous creates", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &ambiguousCreateStore{Store: memory}
		if err := initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("initialize with ambiguous creates: %v", err)
		}
		if _, err := Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID}); err != nil {
			t.Fatalf("open reconciled world: %v", err)
		}
	})

	t.Run("ambiguous before immutable commit", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &failFirstCreateStore{Store: memory, category: blob.ErrAmbiguous}
		if err := initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("initialize after ambiguous create: %v", err)
		}
	})

	t.Run("throttled before immutable commit", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &failFirstCreateStore{Store: memory, category: blob.ErrThrottled}
		if err := initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("initialize after throttled create: %v", err)
		}
	})

	t.Run("ambiguous before head commit", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &failFirstCreateStore{Store: memory, prefix: markerKey, category: blob.ErrAmbiguous}
		if err := initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("initialize after ambiguous head create: %v", err)
		}
	})

	t.Run("preexisting immutable bytes", func(t *testing.T) {
		memory := newTestMemory(t)
		model, err := buildGenesis(testWorldID)
		if err != nil {
			t.Fatalf("build genesis: %v", err)
		}
		first := model[0]
		if _, err := memory.Create(context.Background(), first.Key, first.Data); err != nil {
			t.Fatalf("seed immutable object: %v", err)
		}
		if err := initialize(context.Background(), memory, testWorldID); err != nil {
			t.Fatalf("initialize around immutable object: %v", err)
		}
	})

	t.Run("conflicting immutable bytes", func(t *testing.T) {
		memory := newTestMemory(t)
		model, err := buildGenesis(testWorldID)
		if err != nil {
			t.Fatalf("build genesis: %v", err)
		}
		first := model[0]
		if _, err := memory.Create(context.Background(), first.Key, []byte("corrupt")); err != nil {
			t.Fatalf("seed corrupt immutable object: %v", err)
		}
		err = initialize(context.Background(), memory, testWorldID)
		if !errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("initialize error = %v, want integrity", err)
		}
	})
}

func TestOpenValidation(t *testing.T) {
	t.Run("missing head", func(t *testing.T) {
		store, err := Open(context.Background(), newTestMemory(t), Options{Logger: discardLogger, WorldID: testWorldID, ReadOnly: true})
		if store != nil || !errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("Open() = (%v, %v), want nil clear not-found", store, err)
		}
	})

	t.Run("empty bucket gets genesis", func(t *testing.T) {
		objects := newTestMemory(t)
		store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("Open() error = %v, want a new world", err)
		}
		if got := store.servedSequence(); got != 1 {
			t.Errorf("served sequence = %d, want genesis at 1", got)
		}
		if _, err := objects.Head(context.Background(), markerKey); err != nil {
			t.Errorf("genesis head: %v", err)
		}
	})

	t.Run("wrong world", func(t *testing.T) {
		objects := initializedMemory(t)
		store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: otherWorldID})
		if store != nil || !errors.Is(err, blob.ErrPrecondition) {
			t.Fatalf("Open() = (%v, %v), want world precondition", store, err)
		}
		if err := initialize(context.Background(), objects, otherWorldID); !errors.Is(err, blob.ErrPrecondition) {
			t.Fatalf("initialize(other world) error = %v, want precondition", err)
		}
	})

	t.Run("invalid inputs", func(t *testing.T) {
		memory := initializedMemory(t)
		var typedNil *blob.Memory
		var nilContext context.Context
		tests := []struct {
			name string
			open func() (*Store, error)
		}{
			{name: "nil context", open: func() (*Store, error) {
				return Open(nilContext, memory, Options{Logger: discardLogger, WorldID: testWorldID})
			}},
			{name: "nil store", open: func() (*Store, error) {
				return Open(context.Background(), nil, Options{Logger: discardLogger, WorldID: testWorldID})
			}},
			{name: "typed nil store", open: func() (*Store, error) {
				return Open(context.Background(), typedNil, Options{Logger: discardLogger, WorldID: testWorldID})
			}},
			{name: "invalid world", open: func() (*Store, error) {
				return Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: "bad"})
			}},
			{name: "negative timeout", open: func() (*Store, error) {
				return Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID, RequestTimeout: -time.Second})
			}},
			{name: "negative workers", open: func() (*Store, error) {
				return Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID, ShardWorkers: -1})
			}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				store, err := test.open()
				if store != nil || !errors.Is(err, blob.ErrPrecondition) {
					t.Errorf("Open() = (%v, %v), want nil precondition", store, err)
				}
			})
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := initialize(ctx, newTestMemory(t), testWorldID); !errors.Is(err, context.Canceled) {
			t.Errorf("initialize() error = %v, want canceled", err)
		}
		store, err := Open(ctx, initializedMemory(t), Options{Logger: discardLogger, WorldID: testWorldID})
		if store != nil || !errors.Is(err, context.Canceled) {
			t.Errorf("Open() = (%v, %v), want nil canceled", store, err)
		}
	})
}

func TestOpenRejectsMalformedMarker(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "malformed", mutate: func([]byte) []byte { return []byte(`{`) }},
		{name: "unknown field", mutate: func(data []byte) []byte {
			return []byte(strings.TrimSuffix(string(data), "}") + `,"unknown":true}`)
		}},
		{name: "trailing newline", mutate: func(data []byte) []byte { return append(bytes.Clone(data), '\n') }},
		{name: "trailing value", mutate: func(data []byte) []byte { return append(bytes.Clone(data), []byte(`{}`)...) }},
		{name: "newer schema", mutate: func([]byte) []byte {
			return []byte(`{"schema":3,"world_id":"` + testWorldID + `"}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := initializedMemory(t)
			head := getObject(t, objects, markerKey)
			replaceObject(t, objects, markerKey, head.Attributes.Generation, test.mutate(head.Data))
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}
}

// A schema 1 world opens only once migrated; until then the store refuses it
// by name, and creates nothing over it.
func TestOpenRefusesSchemaOneWorld(t *testing.T) {
	objects := newTestMemory(t)
	legacy := []byte(`{"schema":1,"world_id":"` + testWorldID + `","sequence":1,"root":{"key":"k","hash":"h"},"receipts":[]}`)
	if _, err := objects.Create(context.Background(), markerKey, legacy); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
	if store != nil || !errors.Is(err, blob.ErrPrecondition) || !strings.Contains(err.Error(), "migrated") {
		t.Fatalf("Open() = (%v, %v), want a precondition naming migration", store, err)
	}
	listed, err := objects.List(context.Background(), "", "", "")
	if err != nil || len(listed.Objects) != 1 {
		t.Fatalf("objects after the refusal = %d, %v; want the head alone", len(listed.Objects), err)
	}
}

// Every break in the log fails the open: a gap, a slot that is not
// canonical, one that names another predecessor, one from another world.
func TestOpenRejectsBrokenLog(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, objects *blob.Memory)
	}{
		{name: "gap", damage: func(t *testing.T, objects *blob.Memory) { deleteObject(t, objects, slotKey(3)) }},
		{name: "corrupt bytes", damage: func(t *testing.T, objects *blob.Memory) {
			slot := getObject(t, objects, slotKey(3))
			replaceObject(t, objects, slotKey(3), slot.Attributes.Generation, append(bytes.Clone(slot.Data), ' '))
		}},
		{name: "broken chain", damage: func(t *testing.T, objects *blob.Memory) {
			rewriteSlot(t, objects, 3, func(slot *slotObject) { slot.Prev = strings.Repeat("0", 64) })
		}},
		{name: "other world", damage: func(t *testing.T, objects *blob.Memory) {
			rewriteSlot(t, objects, 3, func(slot *slotObject) { slot.WorldID = otherWorldID })
		}},
		{name: "version skips", damage: func(t *testing.T, objects *blob.Memory) {
			rewriteSlot(t, objects, 3, func(slot *slotObject) {
				slot.Entries[0].Current, slot.Entries[0].Version.Version = 3, 3
			})
		}},
		{name: "retention moves back", damage: func(t *testing.T, objects *blob.Memory) {
			rewriteSlot(t, objects, 5, func(slot *slotObject) { slot.Entries[0].First = 1 })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := initializedMemory(t)
			writer := (&bucketSite{objects: objects}).open(t, 0)
			for version := range 4 {
				var meta map[string]string
				if version == 2 {
					meta = map[string]string{"retention": "2"}
				}
				if _, err := writer.WriteVersion("/docs/a.md", version, fmt.Appendf(nil, "# v%d\n", version+1), meta); err != nil {
					t.Fatal(err)
				}
			}
			test.damage(t, objects)
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}
}

func TestOpenRejectsMissingOrCorruptReferences(t *testing.T) {
	tests := []struct {
		name   string
		legacy bool // the newest checkpoint names a schema 1 root
		mutate func(*testing.T, *blob.Memory)
	}{
		{name: "missing root", mutate: func(t *testing.T, objects *blob.Memory) {
			checkpoint, _ := readFoldedRoot(t, objects)
			deleteObject(t, objects, checkpoint.Root.Key)
		}},
		{name: "missing shard", mutate: func(t *testing.T, objects *blob.Memory) {
			_, root := readFoldedRoot(t, objects)
			deleteObject(t, objects, root.Shards[0].Key)
		}},
		{name: "missing schema 1 shard", legacy: true, mutate: func(t *testing.T, objects *blob.Memory) {
			_, root := readCheckpointAndRoot(t, objects)
			deleteObject(t, objects, root.Shards[37].Key)
		}},
		{name: "corrupt root bytes", mutate: func(t *testing.T, objects *blob.Memory) {
			checkpoint, _ := readFoldedRoot(t, objects)
			root := getObject(t, objects, checkpoint.Root.Key)
			replaceObject(t, objects, checkpoint.Root.Key, root.Attributes.Generation, append(bytes.Clone(root.Data), ' '))
		}},
		{name: "no checkpoint", mutate: func(t *testing.T, objects *blob.Memory) {
			deleteObject(t, objects, checkpointKey(1))
		}},
		{name: "corrupt checkpoint", mutate: func(t *testing.T, objects *blob.Memory) {
			checkpoint := getObject(t, objects, checkpointKey(1))
			replaceObject(t, objects, checkpointKey(1), checkpoint.Attributes.Generation, append(bytes.Clone(checkpoint.Data), ' '))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := initializedMemory(t)
			if test.legacy {
				objects = legacyMemory(t)
			}
			test.mutate(t, objects)
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, ShardWorkers: 4})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
			if strings.HasPrefix(test.name, "missing") && !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("missing reference error does not preserve not-found cause: %v", err)
			}
		})
	}
}

func TestOpenRejectsRootInvariants(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*rootObject)
	}{
		{name: "null shards", mutate: func(root *rootObject) { root.Shards = nil }},
		{name: "wrong shard order", mutate: func(root *rootObject) { root.Shards[0], root.Shards[1] = root.Shards[1], root.Shards[0] }},
		{name: "too many documents", mutate: func(root *rootObject) { root.DocumentCount = maximumDocuments + 1 }},
		{name: "wrong world", mutate: func(root *rootObject) { root.WorldID = otherWorldID }},
		{name: "document count mismatch", mutate: func(root *rootObject) { root.DocumentCount = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := legacyMemory(t)
			_, root := readCheckpointAndRoot(t, objects)
			test.mutate(&root)
			installRoot(t, objects, root)
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}
}

func TestOpenRejectsShardInvariants(t *testing.T) {
	shardIndex, paths := findPathsInShard(t, 2)
	wrongShard := fmt.Sprintf("%02x", (shardIndex+1)%shardCount)
	first := testEntry(paths[0], false, "")
	second := testEntry(paths[1], false, "")
	if first.Path > second.Path {
		first, second = second, first
	}
	tests := []struct {
		name    string
		shard   shardObject
		count   int
		entries []shardEntry
	}{
		{name: "null entries", shard: shardObject{Schema: schemaVersion, Shard: fmt.Sprintf("%02x", shardIndex)}},
		{name: "wrong label", shard: shardObject{Schema: schemaVersion, Shard: wrongShard, Entries: make([]shardEntry, 0)}},
		{name: "unsorted entries", count: 2, shard: shardObject{
			Schema: schemaVersion, Shard: fmt.Sprintf("%02x", shardIndex), Entries: []shardEntry{second, first},
		}},
		{name: "bad path hash", count: 1, entries: []shardEntry{first}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := legacyMemory(t)
			shard := test.shard
			if test.entries != nil {
				shard = shardObject{
					Schema:  schemaVersion,
					Shard:   fmt.Sprintf("%02x", shardIndex),
					Entries: slices.Clone(test.entries),
				}
				shard.Entries[0].PathHash = strings.Repeat("0", 64)
			}
			installShard(t, objects, shardIndex, shard, test.count)
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}

	t.Run("document ancestor", func(t *testing.T) {
		objects := legacyMemory(t)
		installEntries(t, objects, []shardEntry{
			testEntry("/a.md", false, ""),
			testEntry("/a.md/b.md", false, ""),
		})
		store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
		if store != nil || !errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("Open() = (%v, %v), want topology integrity", store, err)
		}
	})
}

func TestDerivedSnapshotLiveAndArchived(t *testing.T) {
	objects := legacyMemory(t)
	sharedHash := storefmt.ContentHash([]byte("shared"))
	archivedHash := storefmt.ContentHash([]byte("archived"))
	installEntries(t, objects, []shardEntry{
		testEntry("/docs/z.md", false, sharedHash),
		testEntry("/docs/a.md", false, sharedHash),
		testEntry("/docs/archived.md", true, sharedHash),
		testEntry("/archive/only.md", true, archivedHash),
	})
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, ShardWorkers: 7})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	loaded := store.served.Load().snap

	t.Run("paths and hashes", func(t *testing.T) {
		if loaded.Paths.Len() != 4 {
			t.Fatalf("paths = %d, want 4", loaded.Paths.Len())
		}
		if !loaded.path("/docs/archived.md").Archived || !loaded.path("/archive/only.md").Archived {
			t.Error("archived paths were not retained")
		}
		if got, _ := loaded.lookupHash(sharedHash); got != "/docs/a.md" {
			t.Errorf("shared body path = %q, want lexicographically smallest live path", got)
		}
		if _, exists := loaded.lookupHash(archivedHash); exists {
			t.Errorf("archived-only body hash %q is live", archivedHash)
		}
	})

	t.Run("catalog", func(t *testing.T) {
		results, err := catalog.Search(loaded, "*", catalog.Options{})
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		paths := make([]string, len(results))
		for index, result := range results {
			paths[index] = result.Path
		}
		sort.Strings(paths)
		if !slices.Equal(paths, []string{"/docs/a.md", "/docs/z.md"}) {
			t.Errorf("catalog paths = %v", paths)
		}
	})

	t.Run("directory topology", func(t *testing.T) {
		archive := directoryChildNamed(loaded, "/", "archive")
		if archive == nil || !archive.IsDir || archive.Live != 0 {
			t.Errorf("archive root child = %+v", archive)
		}
		docs := directoryChildNamed(loaded, "/", "docs")
		if docs == nil || !docs.IsDir || docs.Live != 2 || docs.Docs != 3 {
			t.Errorf("docs root child = %+v", docs)
		}
		only := directoryChildNamed(loaded, "/archive", "only.md")
		if only == nil || only.IsDir || only.Live != 0 || only.Visible != 1 {
			t.Errorf("archived document child = %+v", only)
		}
	})
}

func directoryChildNamed(loaded *snapshot, parent, name string) *dirChild {
	child, _ := loaded.Children.Get(&dirChild{Parent: parent, Name: name})
	return child
}

func TestShardWorkerTimeout(t *testing.T) {
	objects := initializedMemory(t)
	blocking := &blockingGetStore{Store: objects, prefix: objectPrefix + "index/"}
	store, err := Open(context.Background(), blocking, Options{
		Logger: discardLogger, WorldID: testWorldID, noHedge: true,
		RequestTimeout: 100 * time.Millisecond,
		ShardWorkers:   5,
	})
	if store != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open() = (%v, %v), want deadline", store, err)
	}
	if active := blocking.active.Load(); active != 0 {
		t.Errorf("active shard reads after Open = %d", active)
	}
	if peak := blocking.peak.Load(); peak < 1 || peak > 5 {
		t.Errorf("peak shard reads = %d, want [1,5]", peak)
	}
}

// failFirstCreateStore fails the first create under prefix before it lands,
// with category, or with a plain error when category is nil.
type failFirstCreateStore struct {
	blob.Store
	prefix   string
	category error
	failed   atomic.Bool
}

func (store *failFirstCreateStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if strings.HasPrefix(key, store.prefix) && store.failed.CompareAndSwap(false, true) {
		if store.category == nil {
			return blob.Attributes{}, errors.New("injected create failure")
		}
		return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: store.category}
	}
	return store.Store.Create(ctx, key, data)
}

// ambiguousCreateStore lands creates under prefix but reports them ambiguous:
// every one, or with once only the first.
type ambiguousCreateStore struct {
	blob.Store
	prefix string
	once   bool
	done   atomic.Bool
}

func (store *ambiguousCreateStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	attributes, err := store.Store.Create(ctx, key, data)
	if err != nil || !strings.HasPrefix(key, store.prefix) || store.once && !store.done.CompareAndSwap(false, true) {
		return attributes, err
	}
	return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: blob.ErrAmbiguous}
}

// blockingGetStore holds every read under prefix until its context ends, and
// counts how many it held at once.
type blockingGetStore struct {
	blob.Store
	prefix string
	active atomic.Int64
	peak   atomic.Int64
}

func (store *blockingGetStore) Get(ctx context.Context, key string) (blob.Object, error) {
	if !strings.HasPrefix(key, store.prefix) {
		return store.Store.Get(ctx, key)
	}
	active := store.active.Add(1)
	defer store.active.Add(-1)
	for {
		peak := store.peak.Load()
		if active <= peak || store.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	<-ctx.Done()
	return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: ctx.Err()}
}

func newTestMemory(t *testing.T) *blob.Memory {
	t.Helper()
	objects, err := blob.NewMemory(1 << 20)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	return objects
}

func initializedMemory(t *testing.T) *blob.Memory {
	t.Helper()
	objects := newTestMemory(t)
	if err := initialize(context.Background(), objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return objects
}

func getObject(t *testing.T, objects blob.Store, key string) blob.Object {
	t.Helper()
	object, err := objects.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	return object
}

func decodeObject(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := decodeImmutable(data, target); err != nil {
		t.Fatalf("decode object: %v", err)
	}
}

func replaceObject(t *testing.T, objects blob.Store, key string, generation blob.Generation, data []byte) {
	t.Helper()
	if _, err := objects.Replace(context.Background(), key, generation, data); err != nil {
		t.Fatalf("replace %q: %v", key, err)
	}
}

func deleteObject(t *testing.T, objects blob.Store, key string) {
	t.Helper()
	object := getObject(t, objects, key)
	if err := objects.Delete(context.Background(), key, object.Attributes.Generation); err != nil {
		t.Fatalf("delete %q: %v", key, err)
	}
}

// readCheckpointAndRoot reads the world's newest checkpoint and its schema 1
// root.
func readCheckpointAndRoot(t *testing.T, objects blob.Store) (checkpointObject, rootObject) {
	t.Helper()
	checkpoint, err := newestCheckpoint(context.Background(), objects, testWorldID)
	if err != nil {
		t.Fatalf("newest checkpoint: %v", err)
	}
	var root rootObject
	decodeObject(t, getObject(t, objects, checkpoint.Root.Key).Data, &root)
	return checkpoint, root
}

// readFoldedRoot reads the world's newest checkpoint and its folded root.
func readFoldedRoot(t *testing.T, objects blob.Store) (checkpointObject, foldedRoot) {
	t.Helper()
	checkpoint, err := newestCheckpoint(context.Background(), objects, testWorldID)
	if err != nil {
		t.Fatalf("newest checkpoint: %v", err)
	}
	var root foldedRoot
	decodeObject(t, getObject(t, objects, checkpoint.Root.Key).Data, &root)
	return checkpoint, root
}

// installRoot makes root the world's newest checkpoint, as a compactor or a
// migration writes one; stores opened afterwards load it.
func installRoot(t *testing.T, objects blob.Store, root any) {
	t.Helper()
	model, ref, err := immutableJSON(rootKey, root)
	if err != nil {
		t.Fatalf("build root: %v", err)
	}
	if _, err := objects.Create(context.Background(), model.Key, model.Data); err != nil {
		t.Fatalf("create root %q: %v", model.Key, err)
	}
	previous, err := newestCheckpointSequence(context.Background(), objects)
	if err != nil {
		t.Fatalf("newest checkpoint: %v", err)
	}
	checkpoint := checkpointObject{Schema: logSchema, WorldID: testWorldID, Sequence: previous + 1, Root: ref}
	data, err := marshalImmutable(checkpoint)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	if _, err := objects.Create(context.Background(), checkpointKey(checkpoint.Sequence), data); err != nil {
		t.Fatalf("create checkpoint %d: %v", checkpoint.Sequence, err)
	}
}

func installShard(t *testing.T, objects blob.Store, index int, shard shardObject, documentCount int) {
	t.Helper()
	_, root := readCheckpointAndRoot(t, objects)
	shardID := fmt.Sprintf("%02x", index)
	model, ref, err := immutableJSON(func(hash string) string { return shardKey(shardID, hash) }, shard)
	if err != nil {
		t.Fatalf("build shard: %v", err)
	}
	if _, err := objects.Create(context.Background(), model.Key, model.Data); err != nil {
		t.Fatalf("create shard %q: %v", model.Key, err)
	}
	root.Shards[index] = shardRef{Shard: shardID, objectRef: ref}
	root.DocumentCount = documentCount
	installRoot(t, objects, root)
}

func installEntries(t *testing.T, objects blob.Store, entries []shardEntry) {
	t.Helper()
	_, root := readCheckpointAndRoot(t, objects)
	grouped := make(map[int][]shardEntry)
	for entryIndex := range entries {
		entry := &entries[entryIndex]
		indexValue, err := strconv.ParseUint(pathHash(entry.Path)[:2], 16, 8)
		if err != nil {
			t.Fatalf("parse shard for %q: %v", entry.Path, err)
		}
		index := int(indexValue)
		grouped[index] = append(grouped[index], *entry)
	}
	for index, shardEntries := range grouped {
		sort.Slice(shardEntries, func(left, right int) bool {
			return shardEntries[left].Path < shardEntries[right].Path
		})
		shardID := fmt.Sprintf("%02x", index)
		shard := shardObject{Schema: schemaVersion, Shard: shardID, Entries: shardEntries}
		model, ref, err := immutableJSON(func(hash string) string { return shardKey(shardID, hash) }, shard)
		if err != nil {
			t.Fatalf("build shard %s: %v", shardID, err)
		}
		if _, err := objects.Create(context.Background(), model.Key, model.Data); err != nil {
			t.Fatalf("create shard %s: %v", shardID, err)
		}
		root.Shards[index] = shardRef{Shard: shardID, objectRef: ref}
	}
	root.DocumentCount = len(entries)
	installRoot(t, objects, root)
}

func testEntry(path string, archived bool, bodyHash string) shardEntry {
	pathSum := pathHash(path)
	manifestSum := hashHex([]byte("manifest:" + path))
	if bodyHash == "" {
		bodyHash = storefmt.ContentHash([]byte(path))
	}
	title := pathpkg.Base(path)
	metadata := map[string]string{
		"importance": "0.8",
		"tags":       "test, storage",
		"title":      title,
		"type":       "Document",
	}
	return shardEntry{
		Path:     path,
		PathHash: pathSum,
		Manifest: objectRef{Key: manifestKey(pathSum, manifestSum), Hash: manifestSum},
		Current:  1,
		Archived: archived,
		BodyHash: bodyHash,
		Modified: testModified,
		Catalog: catalogRecord{
			Path:       path,
			Title:      title,
			Tags:       []string{"test", "storage"},
			Importance: "0.8",
			Modified:   testModified,
			Metadata:   metadata,
		},
	}
}

func findPathsInShard(t *testing.T, count int) (shardIndex int, paths []string) {
	t.Helper()
	grouped := make(map[int][]string)
	for index := range 10_000 {
		path := fmt.Sprintf("/docs/item-%04d.md", index)
		shardValue, err := strconv.ParseUint(pathHash(path)[:2], 16, 8)
		if err != nil {
			t.Fatalf("parse shard: %v", err)
		}
		index := int(shardValue)
		grouped[index] = append(grouped[index], path)
		if len(grouped[index]) == count {
			return index, grouped[index]
		}
	}
	t.Fatalf("no shard with %d paths", count)
	return 0, nil
}
