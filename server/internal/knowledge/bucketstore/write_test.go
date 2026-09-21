package bucketstore

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/storetest"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
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
		return storetest.LookupBackend{Store: store}
	})
}

func TestLookupHandlerConformance(t *testing.T) {
	storetest.RunLookupHandlerConformance(t, func(t *testing.T) storetest.LookupBackend {
		store, _ := newWritableStore(t)
		return storetest.LookupBackend{Store: store}
	})
}

func TestHandlerConformance(t *testing.T) {
	storetest.RunHandlerConformance(t, func(t *testing.T) storetest.LookupBackend {
		store, _ := newWritableStore(t)
		return storetest.LookupBackend{Store: store}
	})
}

func TestRejectionConformance(t *testing.T) {
	open := func(t *testing.T, options Options) storetest.LookupBackend {
		t.Helper()
		objects := initializedMemory(t)
		options.WorldID, options.Logger = testWorldID, discardLogger
		store, err := Open(context.Background(), objects, options)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		store.commitInterval = 0
		return storetest.LookupBackend{Store: store}
	}
	storetest.RunRejectionConformance(t, storetest.RejectionFactories{
		Quota: func(t *testing.T) storetest.LookupBackend { return open(t, Options{MaxDocuments: 2}) },
		Policy: func(t *testing.T) storetest.LookupBackend {
			seed := writepolicy.PolicySeed{
				Body:     []byte("# Write Policy\n\nCurated.\n\nstrictness: block\nrequire_tags: domain\n"),
				Metadata: policyMetadata(),
			}
			b := open(t, Options{})
			if _, err := writepolicy.Seed(context.Background(), b.Store, seed); err != nil {
				t.Fatalf("seed policy: %v", err)
			}
			b.Store = writepolicy.Enforce(b.Store, writepolicy.Options{Require: true})
			return b
		},
	})
}

// TestWireGoldens holds the bucket backend to the wire fixtures the file
// backend emits; regenerate them from storetest, never from here.
func TestWireGoldens(t *testing.T) {
	store, _ := newWritableStore(t)
	backend := storetest.LookupBackend{Store: store}
	storetest.RunWireGoldens(t, backend, "../../../../protocol/wiretest/testdata", false)
}

func TestFileDifferential(t *testing.T) {
	storetest.RunDifferential(t,
		func(t *testing.T) storetest.LookupBackend { return storetest.FileBackend(t) },
		func(t *testing.T) storetest.LookupBackend {
			store, _ := newWritableStore(t)
			return storetest.LookupBackend{Store: store}
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
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
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
	entry, exists := loaded.Paths[storefmt.CanonicalPath(path)]
	if !exists {
		t.Fatalf("tamper path %s is missing", path)
	}
	view := &readView{objects: store.objects, snapshot: loaded}
	history, err := view.loadHistory(context.Background(), &entry)
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
