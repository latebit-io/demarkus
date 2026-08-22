package bucketstore

import (
	"context"
	"testing"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

func TestStoreConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) handler.DocumentStore {
		store, _ := newWritableStore(t)
		return store
	}, tamperBucketVersion)
}

func TestLookupConformance(t *testing.T) {
	storetest.RunLookupConformance(t, func(t *testing.T) storetest.LookupBackend {
		store, _ := newWritableStore(t)
		return storetest.LookupBackend{Store: store, Catalog: store, Views: store}
	})
}

func TestFileDifferential(t *testing.T) {
	storetest.RunDifferential(t,
		func(t *testing.T) storetest.LookupBackend { return storetest.FileBackend(t) },
		func(t *testing.T) storetest.LookupBackend {
			store, _ := newWritableStore(t)
			return storetest.LookupBackend{Store: store, Catalog: store, Views: store}
		},
		storetest.DifferentialConfig{Seeds: []int64{1, 2}, Ops: 80},
	)
}

func newWritableStore(t testing.TB) (*Store, *blob.Memory) {
	t.Helper()
	objects, err := blob.NewMemory(4 << 20)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	if err := Initialize(context.Background(), objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	store, err := Open(context.Background(), objects, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.commitInterval = 0
	return store, objects
}

func tamperBucketVersion(t testing.TB, documentStore handler.DocumentStore, path string, version int, stored []byte) {
	t.Helper()
	store, ok := documentStore.(*Store)
	if !ok {
		t.Fatalf("tamper store is %T, want *bucketstore.Store", documentStore)
	}
	loaded := store.snapshot.Load()
	entry, exists := loaded.Paths[protocolstore.CanonicalPath(path)]
	if !exists {
		t.Fatalf("tamper path %s is missing", path)
	}
	view := &readView{ctx: context.Background(), cancel: func() {}, objects: store.objects, snapshot: loaded}
	history, err := view.loadHistory(&entry)
	if err != nil {
		t.Fatalf("tamper load history: %v", err)
	}
	retained, exists := retainedAt(history.versions, version)
	if !exists {
		t.Fatalf("tamper version %s v%d is missing", path, version)
	}
	object, err := store.objects.Get(context.Background(), retained.entry.Blob.Key)
	if err != nil {
		t.Fatalf("tamper get blob: %v", err)
	}
	if _, err := store.objects.Replace(context.Background(), retained.entry.Blob.Key, object.Attributes.Generation, stored); err != nil {
		t.Fatalf("tamper replace blob: %v", err)
	}
}

func TestRandomOperationID(t *testing.T) {
	for range 100 {
		operationID, err := randomOperationID()
		if err != nil {
			t.Fatalf("random operation ID: %v", err)
		}
		if !validWorldID(operationID) || operationID[14] != '4' {
			t.Errorf("operation ID %q is not RFC UUIDv4", operationID)
		}
	}
}
