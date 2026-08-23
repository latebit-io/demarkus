package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

func TestInitializeGenesis(t *testing.T) {
	objects := newTestMemory(t)
	if err := Initialize(context.Background(), objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	t.Run("object graph", func(t *testing.T) {
		listed, err := objects.List(context.Background(), "", "")
		if err != nil {
			t.Fatalf("list objects: %v", err)
		}
		if len(listed.Objects) != shardCount+2 || listed.NextCursor != "" {
			t.Fatalf("object count = %d cursor %q, want %d and empty", len(listed.Objects), listed.NextCursor, shardCount+2)
		}
		headValue := getObject(t, objects, headObjectKey)
		var head headObject
		decodeObject(t, headValue.Data, &head)
		if head.Schema != schemaVersion || head.WorldID != testWorldID || head.Sequence != 1 || len(head.Receipts) != 0 || head.Receipts == nil {
			t.Fatalf("head = %+v", head)
		}
		if !bytes.Contains(headValue.Data, []byte(`"receipts":[]`)) {
			t.Errorf("head does not encode empty receipts as []: %s", headValue.Data)
		}

		rootValue := getObject(t, objects, head.Root.Key)
		if hashHex(rootValue.Data) != head.Root.Hash || head.Root.Key != rootKey(head.Root.Hash) {
			t.Fatalf("root identity does not match head: %+v", head.Root)
		}
		var root rootObject
		decodeObject(t, rootValue.Data, &root)
		if root.DocumentCount != 0 || len(root.Shards) != shardCount || root.Shards == nil {
			t.Fatalf("root count=%d shards=%d nil=%v", root.DocumentCount, len(root.Shards), root.Shards == nil)
		}
		for index, ref := range root.Shards {
			wantShard := fmt.Sprintf("%02x", index)
			if ref.Shard != wantShard || ref.Key != shardKey(wantShard, ref.Hash) {
				t.Fatalf("shard ref %d = %+v", index, ref)
			}
			value := getObject(t, objects, ref.Key)
			if hashHex(value.Data) != ref.Hash {
				t.Fatalf("shard %s hash mismatch", wantShard)
			}
			var shard shardObject
			decodeObject(t, value.Data, &shard)
			if shard.Entries == nil || len(shard.Entries) != 0 || !bytes.Contains(value.Data, []byte(`"entries":[]`)) {
				t.Fatalf("shard %s entries = %#v bytes=%s", wantShard, shard.Entries, value.Data)
			}
		}
	})

	t.Run("open defaults", func(t *testing.T) {
		store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if store.requestTimeout != defaultRequestTimeout || store.shardWorkers != defaultShardWorkers {
			t.Errorf("defaults timeout=%v workers=%d", store.requestTimeout, store.shardWorkers)
		}
		loaded := store.snapshot.Load()
		if loaded == nil || len(loaded.Paths) != 0 || len(loaded.BodyHashes) != 0 || loaded.Catalog.Len() != 0 {
			t.Fatalf("empty snapshot = %+v", loaded)
		}
		root, exists := loaded.Directories["/"]
		if !exists || root.Children == nil || len(root.Children) != 0 {
			t.Errorf("root directory = %+v, exists %v", root, exists)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		before := getObject(t, objects, headObjectKey).Attributes.Generation
		if err := Initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("reinitialize: %v", err)
		}
		after := getObject(t, objects, headObjectKey).Attributes.Generation
		if after != before {
			t.Errorf("head generation changed from %d to %d", before, after)
		}
		listed, err := objects.List(context.Background(), "", "")
		if err != nil {
			t.Fatalf("list after reinitialize: %v", err)
		}
		if len(listed.Objects) != shardCount+2 {
			t.Errorf("object count after reinitialize = %d", len(listed.Objects))
		}
	})
}

func TestOpenRequiresValidPolicy(t *testing.T) {
	objects := initializedMemory(t)
	if _, err := Open(context.Background(), objects, Options{WorldID: testWorldID, RequirePolicy: true}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("Open without policy error = %v, want ErrInvalidPolicy", err)
	}

	store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedPolicy(t, store, "strictness: block\n", 0)
	if _, err := Open(context.Background(), objects, Options{WorldID: testWorldID, RequirePolicy: true}); err != nil {
		t.Fatalf("Open with policy: %v", err)
	}
}

func TestSeparateBucketsAreIndependent(t *testing.T) {
	const otherWorldID = "7d4f3f8a-87f0-4bf5-932a-e4d1db28d235"
	open := func(worldID string) *Store {
		objects := newTestMemory(t)
		if err := Initialize(context.Background(), objects, worldID); err != nil {
			t.Fatalf("initialize %s: %v", worldID, err)
		}
		store, err := Open(context.Background(), objects, Options{WorldID: worldID})
		if err != nil {
			t.Fatalf("open %s: %v", worldID, err)
		}
		store.commitInterval = 0
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
	if _, err := left.LookupHashResult(protocolstore.ContentHash(body)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("left shared hash error = %v, want ErrNotExist", err)
	}
	if path, err := right.LookupHashResult(protocolstore.ContentHash(body)); err != nil || path != "/same" {
		t.Fatalf("right shared hash = (%q, %v), want /same", path, err)
	}
}

func TestInitializeReconciliation(t *testing.T) {
	t.Run("ambiguous creates", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &ambiguousCreateStore{Store: memory}
		if err := Initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("initialize with ambiguous creates: %v", err)
		}
		if _, err := Open(context.Background(), memory, Options{WorldID: testWorldID}); err != nil {
			t.Fatalf("open reconciled world: %v", err)
		}
	})

	t.Run("ambiguous before immutable commit", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &failFirstCreateStore{Store: memory, category: blob.ErrAmbiguous}
		if err := Initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("initialize after ambiguous create: %v", err)
		}
	})

	t.Run("throttled before immutable commit", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &failFirstCreateStore{Store: memory, category: blob.ErrThrottled}
		if err := Initialize(context.Background(), objects, testWorldID); err != nil {
			t.Fatalf("initialize after throttled create: %v", err)
		}
	})

	t.Run("ambiguous before head commit", func(t *testing.T) {
		memory := newTestMemory(t)
		objects := &failHeadCreateStore{Store: memory}
		if err := Initialize(context.Background(), objects, testWorldID); err != nil {
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
		if err := Initialize(context.Background(), memory, testWorldID); err != nil {
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
		err = Initialize(context.Background(), memory, testWorldID)
		if !errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("initialize error = %v, want integrity", err)
		}
	})
}

func TestOpenValidation(t *testing.T) {
	t.Run("missing head", func(t *testing.T) {
		store, err := Open(context.Background(), newTestMemory(t), Options{WorldID: testWorldID})
		if store != nil || !errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("Open() = (%v, %v), want nil clear not-found", store, err)
		}
	})

	t.Run("wrong world", func(t *testing.T) {
		objects := initializedMemory(t)
		store, err := Open(context.Background(), objects, Options{WorldID: otherWorldID})
		if store != nil || !errors.Is(err, blob.ErrPrecondition) {
			t.Fatalf("Open() = (%v, %v), want world precondition", store, err)
		}
		if err := Initialize(context.Background(), objects, otherWorldID); !errors.Is(err, blob.ErrPrecondition) {
			t.Fatalf("Initialize(other world) error = %v, want precondition", err)
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
			{name: "nil context", open: func() (*Store, error) { return Open(nilContext, memory, Options{WorldID: testWorldID}) }},
			{name: "nil store", open: func() (*Store, error) { return Open(context.Background(), nil, Options{WorldID: testWorldID}) }},
			{name: "typed nil store", open: func() (*Store, error) { return Open(context.Background(), typedNil, Options{WorldID: testWorldID}) }},
			{name: "invalid world", open: func() (*Store, error) { return Open(context.Background(), memory, Options{WorldID: "bad"}) }},
			{name: "negative timeout", open: func() (*Store, error) {
				return Open(context.Background(), memory, Options{WorldID: testWorldID, RequestTimeout: -time.Second})
			}},
			{name: "negative workers", open: func() (*Store, error) {
				return Open(context.Background(), memory, Options{WorldID: testWorldID, ShardWorkers: -1})
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
		if err := Initialize(ctx, newTestMemory(t), testWorldID); !errors.Is(err, context.Canceled) {
			t.Errorf("Initialize() error = %v, want canceled", err)
		}
		store, err := Open(ctx, initializedMemory(t), Options{WorldID: testWorldID})
		if store != nil || !errors.Is(err, context.Canceled) {
			t.Errorf("Open() = (%v, %v), want nil canceled", store, err)
		}
	})
}

func TestOpenRejectsMalformedHead(t *testing.T) {
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
		{name: "null receipts", mutate: func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"receipts":[]`, `"receipts":null`, 1))
		}},
		{name: "invalid receipt", mutate: func(data []byte) []byte {
			var head headObject
			decodeObject(t, data, &head)
			head.Sequence = 2
			head.Receipts = []operationReceipt{{OperationID: otherWorldID, Sequence: 2, Result: "failed"}}
			encoded, err := marshalImmutable(head)
			if err != nil {
				t.Fatalf("marshal invalid receipt head: %v", err)
			}
			return encoded
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := initializedMemory(t)
			head := getObject(t, objects, headObjectKey)
			replaceObject(t, objects, headObjectKey, head.Attributes.Generation, test.mutate(head.Data))
			store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}
}

func TestOpenRejectsMissingOrCorruptReferences(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *blob.Memory)
	}{
		{name: "missing root", mutate: func(t *testing.T, objects *blob.Memory) {
			head, _ := readHeadAndRoot(t, objects)
			deleteObject(t, objects, head.Root.Key)
		}},
		{name: "missing shard", mutate: func(t *testing.T, objects *blob.Memory) {
			_, root := readHeadAndRoot(t, objects)
			deleteObject(t, objects, root.Shards[37].Key)
		}},
		{name: "corrupt root bytes", mutate: func(t *testing.T, objects *blob.Memory) {
			head, _ := readHeadAndRoot(t, objects)
			root := getObject(t, objects, head.Root.Key)
			replaceObject(t, objects, head.Root.Key, root.Attributes.Generation, append(bytes.Clone(root.Data), ' '))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := initializedMemory(t)
			test.mutate(t, objects)
			store, err := Open(context.Background(), objects, Options{WorldID: testWorldID, ShardWorkers: 4})
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
			objects := initializedMemory(t)
			_, root := readHeadAndRoot(t, objects)
			test.mutate(&root)
			installRoot(t, objects, root)
			store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
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
			objects := initializedMemory(t)
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
			store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
			if store != nil || !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Open() = (%v, %v), want nil integrity", store, err)
			}
		})
	}

	t.Run("document ancestor", func(t *testing.T) {
		objects := initializedMemory(t)
		installEntries(t, objects, []shardEntry{
			testEntry("/a.md", false, ""),
			testEntry("/a.md/b.md", false, ""),
		})
		store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
		if store != nil || !errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("Open() = (%v, %v), want topology integrity", store, err)
		}
	})
}

func TestDerivedSnapshotLiveAndArchived(t *testing.T) {
	objects := initializedMemory(t)
	sharedHash := protocolstore.ContentHash([]byte("shared"))
	archivedHash := protocolstore.ContentHash([]byte("archived"))
	installEntries(t, objects, []shardEntry{
		testEntry("/docs/z.md", false, sharedHash),
		testEntry("/docs/a.md", false, sharedHash),
		testEntry("/docs/archived.md", true, sharedHash),
		testEntry("/archive/only.md", true, archivedHash),
	})
	store, err := Open(context.Background(), objects, Options{WorldID: testWorldID, ShardWorkers: 7})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	loaded := store.snapshot.Load()

	t.Run("paths and hashes", func(t *testing.T) {
		if len(loaded.Paths) != 4 {
			t.Fatalf("paths = %d, want 4", len(loaded.Paths))
		}
		if !loaded.Paths["/docs/archived.md"].Archived || !loaded.Paths["/archive/only.md"].Archived {
			t.Errorf("archived paths were not retained: %+v", loaded.Paths)
		}
		if got := loaded.BodyHashes[sharedHash]; got != "/docs/a.md" {
			t.Errorf("shared body path = %q, want lexicographically smallest live path", got)
		}
		if _, exists := loaded.BodyHashes[archivedHash]; exists {
			t.Errorf("archived-only body hash %q is live", archivedHash)
		}
	})

	t.Run("catalog", func(t *testing.T) {
		if loaded.Catalog.Len() != 2 {
			t.Fatalf("catalog length = %d, want 2", loaded.Catalog.Len())
		}
		results, err := loaded.Catalog.Lookup("*", catalog.Options{})
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
		root := loaded.Directories["/"]
		archive, exists := directoryChildNamed(root, "archive")
		if !exists || !archive.IsDir || archive.Live {
			t.Errorf("archive root child = %+v, exists %v", archive, exists)
		}
		docs, exists := directoryChildNamed(root, "docs")
		if !exists || !docs.IsDir || !docs.Live {
			t.Errorf("docs root child = %+v, exists %v", docs, exists)
		}
		only, exists := directoryChildNamed(loaded.Directories["/archive"], "only.md")
		if !exists || only.IsDir || only.Live {
			t.Errorf("archived document child = %+v, exists %v", only, exists)
		}
	})
}

func TestShardWorkerTimeout(t *testing.T) {
	objects := initializedMemory(t)
	blocking := &blockingShardStore{Store: objects}
	store, err := Open(context.Background(), blocking, Options{
		WorldID:        testWorldID,
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

type ambiguousCreateStore struct {
	blob.Store
}

type failFirstCreateStore struct {
	blob.Store
	failed   atomic.Bool
	category error
}

func (store *failFirstCreateStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if store.failed.CompareAndSwap(false, true) {
		return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: store.category}
	}
	return store.Store.Create(ctx, key, data)
}

type failHeadCreateStore struct {
	blob.Store
	failed atomic.Bool
}

func (store *failHeadCreateStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if key == headObjectKey && store.failed.CompareAndSwap(false, true) {
		return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: blob.ErrAmbiguous}
	}
	return store.Store.Create(ctx, key, data)
}

func (store *ambiguousCreateStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	attributes, err := store.Store.Create(ctx, key, data)
	if err != nil {
		return attributes, err
	}
	return attributes, &blob.OpError{Op: "create", Key: key, Err: blob.ErrAmbiguous}
}

type blockingShardStore struct {
	blob.Store
	active atomic.Int64
	peak   atomic.Int64
}

func (store *blockingShardStore) Get(ctx context.Context, key string) (blob.Object, error) {
	if !strings.HasPrefix(key, objectPrefix+"index/") {
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
	if err := Initialize(context.Background(), objects, testWorldID); err != nil {
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

func readHeadAndRoot(t *testing.T, objects blob.Store) (headObject, rootObject) {
	t.Helper()
	var head headObject
	decodeObject(t, getObject(t, objects, headObjectKey).Data, &head)
	var root rootObject
	decodeObject(t, getObject(t, objects, head.Root.Key).Data, &root)
	return head, root
}

func installRoot(t *testing.T, objects blob.Store, root rootObject) {
	t.Helper()
	model, ref, err := immutableJSON(rootKey, root)
	if err != nil {
		t.Fatalf("build root: %v", err)
	}
	if _, err := objects.Create(context.Background(), model.Key, model.Data); err != nil {
		t.Fatalf("create root %q: %v", model.Key, err)
	}
	headValue := getObject(t, objects, headObjectKey)
	var head headObject
	decodeObject(t, headValue.Data, &head)
	head.Sequence++
	head.Root = ref
	head.Receipts = append(head.Receipts, operationReceipt{
		OperationID: fmt.Sprintf("00000000-0000-4000-8000-%012x", head.Sequence),
		Sequence:    head.Sequence,
		Result:      "committed",
	})
	if len(head.Receipts) > maximumReceipts {
		head.Receipts = head.Receipts[len(head.Receipts)-maximumReceipts:]
	}
	data, err := marshalImmutable(head)
	if err != nil {
		t.Fatalf("marshal head: %v", err)
	}
	replaceObject(t, objects, headObjectKey, headValue.Attributes.Generation, data)
}

func installShard(t *testing.T, objects blob.Store, index int, shard shardObject, documentCount int) {
	t.Helper()
	_, root := readHeadAndRoot(t, objects)
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
	_, root := readHeadAndRoot(t, objects)
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
		bodyHash = protocolstore.ContentHash([]byte(path))
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

func directoryChildNamed(node directoryNode, name string) (directoryChild, bool) {
	for _, child := range node.Children {
		if child.Name == name {
			return child, true
		}
	}
	return directoryChild{}, false
}
