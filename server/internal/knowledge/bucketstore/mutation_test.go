package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// Two stores that built on one tip race for one slot name: one wins, the
// other reads the winner, rebuilds on it and commits the next slot, or gets
// the answer the winner's change implies.
func TestSlotRace(t *testing.T) {
	t.Run("same path conflicts", func(t *testing.T) {
		left, right, barrier := concurrentStores(t)
		results := runConcurrentWrites(left, "/same", right, "/same")
		barrier.releaseBoth(t)
		assertOneConflict(t, collectWriteOutcomes(t, results))
		assertCurrentVersion(t, left, "/same", 1)
	})

	t.Run("blind same path conflicts after rebase", func(t *testing.T) {
		base, memory := newWritableStore(t)
		if _, err := base.WriteVersion("/same", 0, []byte("base"), nil); err != nil {
			t.Fatalf("seed base: %v", err)
		}
		left, right, barrier := concurrentStoresOn(t, memory)
		results := make(chan writeOutcome, 2)
		go func() {
			document, err := left.WriteVersion("/same", -1, []byte("left"), nil)
			results <- writeOutcome{document: document, err: err}
		}()
		go func() {
			document, err := right.WriteVersion("/same", -1, []byte("right"), nil)
			results <- writeOutcome{document: document, err: err}
		}()
		barrier.releaseBoth(t)
		assertOneConflict(t, collectWriteOutcomes(t, results))
		assertCurrentVersion(t, left, "/same", 2)
	})

	t.Run("rebase without conflict", func(t *testing.T) {
		left, right, barrier := concurrentStores(t)
		results := runConcurrentWrites(left, "/left", right, "/right")
		barrier.releaseBoth(t)
		for _, outcome := range collectWriteOutcomes(t, results) {
			if outcome.err != nil || outcome.document == nil || outcome.document.Version != 1 {
				t.Fatalf("write outcome = (%+v, %v), want v1 success", outcome.document, outcome.err)
			}
		}
		assertCurrentVersion(t, left, "/left", 1)
		assertCurrentVersion(t, left, "/right", 1)
		if got := left.servedSequence(); got != 3 {
			t.Errorf("log tip = %d, want two slots after genesis", got)
		}
		if n := barrier.losses.Load(); n != 1 {
			t.Errorf("lost slot creates = %d, want 1", n)
		}
	})

	t.Run("parent and child topology", func(t *testing.T) {
		left, right, barrier := concurrentStores(t)
		results := runConcurrentWrites(left, "/a", right, "/a/b")
		barrier.releaseBoth(t)
		succeeded, collided := 0, 0
		for _, outcome := range collectWriteOutcomes(t, results) {
			switch {
			case outcome.err == nil:
				succeeded++
			case errors.Is(outcome.err, storefmt.ErrPathCollision):
				collided++
			default:
				t.Errorf("unexpected write error: %v", outcome.err)
			}
		}
		if succeeded != 1 || collided != 1 {
			t.Fatalf("topology outcomes success=%d collision=%d, want 1/1", succeeded, collided)
		}
		parent, err := left.CurrentVersionResult("/a")
		if err != nil {
			t.Fatalf("read /a current version: %v", err)
		}
		assertCurrentVersion(t, left, "/a/b", 1-parent)
	})

	t.Run("retention write", func(t *testing.T) {
		base, memory := newWritableStore(t)
		if _, err := base.WriteVersion("/doc", 0, []byte("base"), nil); err != nil {
			t.Fatalf("seed base: %v", err)
		}
		left, right, barrier := concurrentStoresOn(t, memory)
		results := make(chan writeOutcome, 2)
		go func() {
			document, err := left.WriteVersion("/doc", 1, []byte("retained"), map[string]string{"retention": "1"})
			results <- writeOutcome{document: document, err: err}
		}()
		go func() {
			document, err := right.WriteVersion("/doc", 1, []byte("ordinary"), nil)
			results <- writeOutcome{document: document, err: err}
		}()
		barrier.releaseBoth(t)
		assertOneConflict(t, collectWriteOutcomes(t, results))
		document, err := left.Get("/doc", 0)
		if err != nil {
			t.Fatalf("Get final document: %v", err)
		}
		versions, err := left.Versions("/doc")
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		wantVersions := 2
		if document.Metadata["retention"] == "1" {
			wantVersions = 1
		}
		if document.Version != 2 || len(versions) != wantVersions {
			t.Fatalf("final document v%d versions=%v, want v2 with %d retained", document.Version, versions, wantVersions)
		}
		if err := left.VerifyChain("/doc"); err != nil {
			t.Fatalf("VerifyChain: %v", err)
		}
	})

	t.Run("policy change", func(t *testing.T) {
		base, memory := newWritableStore(t)
		seedPolicy(t, base, "strictness: warn\nrequire_tags: domain\nrequire_fields: title\n", 0)
		blocking := newBlockingFirstSlotStore(memory)
		writer, err := Open(context.Background(), blocking, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open writer: %v", err)
		}
		policyWriter, err := Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open policy writer: %v", err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := enforced(writer, false).WriteVersion("/doc", 0, []byte("body"), nil)
			result <- err
		}()
		waitForTestSignal(t, blocking.blocked, "blocked slot create")
		seedPolicy(t, policyWriter, "strictness: block\nrequire_tags: domain\nrequire_fields: title\n", 1)
		close(blocking.release)
		select {
		case err := <-result:
			if !errors.Is(err, writepolicy.ErrPolicyBlocked) {
				t.Fatalf("rebased write error = %v, want policy block", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for rebased policy result")
		}
		assertCurrentVersion(t, policyWriter, "/doc", 0)
	})

	t.Run("archive rebases onto the winning write", func(t *testing.T) {
		base, memory := newWritableStore(t)
		if _, err := base.WriteVersion("/doc", 0, []byte("v1"), nil); err != nil {
			t.Fatalf("write v1: %v", err)
		}
		blocking := newBlockingFirstSlotStore(memory)
		archiver, err := Open(context.Background(), blocking, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open archiver: %v", err)
		}
		type archiveOutcome struct {
			document *storefmt.Document
			changed  bool
			err      error
		}
		result := make(chan archiveOutcome, 1)
		go func() {
			document, changed, err := archiver.ArchiveResult("/doc", true)
			result <- archiveOutcome{document: document, changed: changed, err: err}
		}()
		waitForTestSignal(t, blocking.blocked, "blocked archive slot")
		written, err := base.WriteVersion("/doc", 1, []byte("v2"), nil)
		if err != nil {
			t.Fatalf("write concurrent v2: %v", err)
		}
		close(blocking.release)
		select {
		case outcome := <-result:
			if outcome.err != nil || !outcome.changed || outcome.document == nil || outcome.document.Version != 2 || !outcome.document.Archived || outcome.document.ETag != written.ETag {
				t.Fatalf("archive outcome = (%+v, changed=%v, err=%v), want archived v2", outcome.document, outcome.changed, outcome.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for archive rebase")
		}
		if err := archiver.VerifyChain("/doc"); err != nil {
			t.Errorf("VerifyChain after archive rebase: %v", err)
		}
	})
}

// A create whose outcome is unknown is decided by reading the slot: absent,
// the identical create is retried; present, its bytes say whose it is.
func TestAmbiguousSlotCreate(t *testing.T) {
	tests := []struct {
		name string
		wrap func(blob.Store) blob.Store
	}{
		{name: "ambiguous before landing", wrap: func(store blob.Store) blob.Store {
			return &failFirstCreateStore{Store: store, prefix: logPrefix, category: blob.ErrAmbiguous}
		}},
		{name: "throttled before landing", wrap: func(store blob.Store) blob.Store {
			return &failFirstCreateStore{Store: store, prefix: logPrefix, category: blob.ErrThrottled}
		}},
		{name: "unavailable before landing", wrap: func(store blob.Store) blob.Store {
			return &failFirstCreateStore{Store: store, prefix: logPrefix, category: blob.ErrUnavailable}
		}},
		{name: "ambiguous after landing", wrap: func(store blob.Store) blob.Store {
			return &ambiguousCreateStore{Store: store, prefix: logPrefix, once: true}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, memory := newWritableStore(t)
			store.objects = test.wrap(memory)
			document, err := store.WriteVersion("/doc", 0, []byte("body"), nil)
			if err != nil || document == nil || document.Version != 1 {
				t.Fatalf("write = (%+v, %v), want v1", document, err)
			}
			assertCurrentVersion(t, store, "/doc", 1)
			if got := store.servedSequence(); got != 2 {
				t.Errorf("log tip = %d, want one slot", got)
			}
			if _, err := store.Get("/missing", 0); !errors.Is(err, backend.ErrNotFound) {
				t.Fatalf("missing read error = %v, want not found", err)
			}
		})
	}

	t.Run("another writer took the name", func(t *testing.T) {
		store, memory := newWritableStore(t)
		peer := (&bucketSite{objects: memory}).open(t, 0)
		store.objects = &peerWinsSlotStore{Store: memory, peer: peer}
		document, err := store.WriteVersion("/mine.md", 0, []byte("mine"), nil)
		if err != nil || document.Version != 1 {
			t.Fatalf("write after losing the name = (%+v, %v), want v1 on the next slot", document, err)
		}
		assertCurrentVersion(t, store, "/peer.md", 1)
		if got := store.servedSequence(); got != 3 {
			t.Errorf("log tip = %d, want the peer's slot and ours", got)
		}
	})

	t.Run("outcome stays unknown", func(t *testing.T) {
		store, memory := newWritableStore(t)
		store.objects = &unreachableSlotStore{Store: memory}
		store.requestTimeout = 2 * time.Second
		if _, err := store.WriteVersion("/doc", 0, []byte("body"), nil); err == nil {
			t.Fatal("a write whose slot can never be read back succeeded")
		}
	})
}

// A failure before the slot leaves the log as it was; a canceled request
// commits nothing.
func TestCommitFailureLeavesTheLog(t *testing.T) {
	t.Run("staging failure", func(t *testing.T) {
		store, memory := newWritableStore(t)
		failing := &failFirstCreateStore{Store: memory, prefix: objectPrefix + "blobs/"}
		store.objects = failing
		if _, err := store.WriteVersion("/doc", 0, []byte("body"), nil); err == nil {
			t.Fatal("write succeeded after an injected staging failure")
		}
		if !failing.failed.Load() {
			t.Fatal("write never reached staging")
		}
		assertNoSlot(t, memory, 2)
		assertCurrentVersion(t, store, "/doc", 0)
	})

	t.Run("request cancellation", func(t *testing.T) {
		store, memory := newWritableStore(t)
		store.requestTimeout = 25 * time.Millisecond
		store.objects = &blockingSlotCreateStore{Store: memory}
		if _, err := store.WriteVersion("/doc", 0, []byte("body"), nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("write error = %v, want deadline", err)
		}
		assertNoSlot(t, memory, 2)
	})
}

// An archive transition writes one slot and nothing else; a no-op writes
// nothing at all.
func TestArchiveTransitions(t *testing.T) {
	store, memory := newWritableStore(t)
	written, err := store.WriteVersion("/doc", 0, []byte("body"), nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	before := countObjects(t, memory)
	archived, changed, err := store.ArchiveResult("/doc", true)
	if err != nil || !changed || !archived.Archived || archived.ETag != written.ETag || !archived.Modified.Equal(written.Modified) {
		t.Fatalf("archive = (%+v, %v, %v)", archived, changed, err)
	}
	if after := countObjects(t, memory); after != before+1 {
		t.Errorf("archive wrote %d objects, want one slot", after-before)
	}
	before = countObjects(t, memory)
	unchanged, changed, err := store.ArchiveResult("/doc", true)
	if err != nil || changed || unchanged.ETag != archived.ETag {
		t.Fatalf("archive no-op = (%+v, %v, %v)", unchanged, changed, err)
	}
	if after := countObjects(t, memory); after != before {
		t.Errorf("archive no-op wrote %d objects", after-before)
	}
	unarchived, changed, err := store.ArchiveResult("/doc", false)
	if err != nil || !changed || unarchived.Archived || unarchived.ETag != written.ETag {
		t.Fatalf("unarchive = (%+v, %v, %v)", unarchived, changed, err)
	}
	if err := store.VerifyChain("/doc"); err != nil {
		t.Fatalf("VerifyChain after unarchive: %v", err)
	}
}

// A view pins its snapshot at its first read: writes after it stay out.
func TestFirstReadPinsTheView(t *testing.T) {
	store, _ := newWritableStore(t)
	if _, err := store.WriteVersion("/doc", 0, []byte("v1"), nil); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	old, err := store.openReadView()
	if err != nil {
		t.Fatalf("open old view: %v", err)
	}
	defer closeTestReadView(t, old)
	if document, err := old.Get("/doc", 0); err != nil || document.Version != 1 {
		t.Fatalf("first read = (%+v, %v), want v1", document, err)
	}
	if _, err := store.WriteVersion("/doc", 1, []byte("v2"), nil); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	oldDocument, err := old.Get("/doc", 0)
	if err != nil || string(oldDocument.Content) != "v1" || oldDocument.Version != 1 {
		t.Errorf("pinned view = (%+v, %v), want v1", oldDocument, err)
	}
	fresh, err := store.Get("/doc", 0)
	if err != nil || string(fresh.Content) != "v2" || fresh.Version != 2 {
		t.Errorf("fresh view = (%+v, %v), want v2", fresh, err)
	}
}

// Retention trims versions held since the checkpoint; the chain stays whole
// across what is left.
func TestRetentionAcrossManyVersions(t *testing.T) {
	store, _ := newWritableStore(t)
	for version := 1; version <= 257; version++ {
		if _, err := store.WriteVersion("/history", version-1, fmt.Appendf(nil, "v%d", version), nil); err != nil {
			t.Fatalf("write v%d: %v", version, err)
		}
	}
	pruned, err := store.WriteVersion("/history", 257, []byte("v258"), map[string]string{"retention": "5"})
	if err != nil {
		t.Fatalf("write retained v258: %v", err)
	}
	if pruned.Prune == nil || pruned.Prune.From != 1 || pruned.Prune.To != 253 {
		t.Fatalf("prune = %+v, want 1-253", pruned.Prune)
	}
	if state := store.served.Load().snap.path("/history"); state.First != 254 || len(state.Recent) != 5 {
		t.Errorf("snapshot keeps first %d and %d versions, want 254 and 5", state.First, len(state.Recent))
	}
	versions, err := store.Versions("/history")
	if err != nil || len(versions) != 5 || versions[0].Version != 258 || versions[4].Version != 254 {
		t.Fatalf("retained versions = (%+v, %v)", versions, err)
	}
	if err := store.VerifyChain("/history"); err != nil {
		t.Fatalf("verify retained chain: %v", err)
	}
	if _, err := store.Get("/history", 253); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("pruned version read = %v, want not found", err)
	}
}

type writeOutcome struct {
	document *storefmt.Document
	err      error
}

func runConcurrentWrites(left *Store, leftPath string, right *Store, rightPath string) <-chan writeOutcome {
	results := make(chan writeOutcome, 2)
	go func() {
		document, err := left.WriteVersion(leftPath, 0, []byte("left"), nil)
		results <- writeOutcome{document: document, err: err}
	}()
	go func() {
		document, err := right.WriteVersion(rightPath, 0, []byte("right"), nil)
		results <- writeOutcome{document: document, err: err}
	}()
	return results
}

func collectWriteOutcomes(t *testing.T, results <-chan writeOutcome) []writeOutcome {
	t.Helper()
	outcomes := make([]writeOutcome, 0, 2)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case outcome := <-results:
			outcomes = append(outcomes, outcome)
		case <-timer.C:
			t.Fatal("timed out waiting for concurrent writes")
		}
	}
	return outcomes
}

func assertOneConflict(t *testing.T, outcomes []writeOutcome) {
	t.Helper()
	succeeded := 0
	conflicted := 0
	for _, outcome := range outcomes {
		switch {
		case outcome.err == nil:
			succeeded++
		case errors.Is(outcome.err, storefmt.ErrConflict):
			conflicted++
		default:
			t.Errorf("unexpected write error: %v", outcome.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Errorf("outcomes success=%d conflict=%d, want 1/1", succeeded, conflicted)
	}
}

func assertCurrentVersion(t *testing.T, store *Store, path string, want int) {
	t.Helper()
	got, err := store.CurrentVersionResult(path)
	if err != nil || got != want {
		t.Fatalf("CurrentVersion(%s) = (%d, %v), want (%d, nil)", path, got, err, want)
	}
}

func assertNoSlot(t *testing.T, objects blob.Store, first int64) {
	t.Helper()
	if _, err := objects.Get(context.Background(), slotKey(first)); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("slot %d after a failed commit: %v, want not found", first, err)
	}
}

func seedPolicy(t *testing.T, store *Store, body string, expected int) {
	t.Helper()
	if _, err := store.WriteVersion(publishpolicy.DocumentPath, expected, []byte(body), policyMetadata()); err != nil {
		t.Fatalf("write policy v%d: %v", expected+1, err)
	}
}

func policyMetadata() map[string]string {
	return map[string]string{"tags": "domain:policy", "title": "Policy"}
}

func countObjects(t *testing.T, store blob.Store) int {
	t.Helper()
	count, cursor := 0, ""
	for {
		listed, err := store.List(context.Background(), "", "", cursor)
		if err != nil {
			t.Fatalf("list objects: %v", err)
		}
		count += len(listed.Objects)
		if listed.NextCursor == "" {
			return count
		}
		cursor = listed.NextCursor
	}
}

func isSlot(key string) bool { return strings.HasPrefix(key, logPrefix) }

// createBarrierStore holds the first two slot creates until both arrived, so
// two stores that built on one tip race for one name. losses counts creates
// that found the name taken.
type createBarrierStore struct {
	blob.Store
	arrived chan struct{}
	release chan struct{}
	calls   atomic.Int64
	losses  atomic.Int64
	once    sync.Once
}

func concurrentStores(t *testing.T) (left, right *Store, barrier *createBarrierStore) {
	t.Helper()
	_, memory := newWritableStore(t)
	return concurrentStoresOn(t, memory)
}

func concurrentStoresOn(t *testing.T, memory blob.Store) (left, right *Store, barrier *createBarrierStore) {
	t.Helper()
	barrier = &createBarrierStore{Store: memory, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	open := func() *Store {
		store, err := Open(context.Background(), barrier, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open concurrent store: %v", err)
		}
		return store
	}
	return open(), open(), barrier
}

func (store *createBarrierStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if isSlot(key) && store.calls.Add(1) <= 2 {
		store.arrived <- struct{}{}
		select {
		case <-store.release:
		case <-ctx.Done():
			return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: ctx.Err()}
		}
	}
	attributes, err := store.Store.Create(ctx, key, data)
	if isSlot(key) && errors.Is(err, blob.ErrPrecondition) {
		store.losses.Add(1)
	}
	return attributes, err
}

func (store *createBarrierStore) releaseBoth(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case <-store.arrived:
		case <-timer.C:
			t.Fatal("timed out waiting for slot contenders")
		}
	}
	store.once.Do(func() { close(store.release) })
}

// blockingFirstSlotStore holds the first slot create until released.
type blockingFirstSlotStore struct {
	blob.Store
	blocked chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func newBlockingFirstSlotStore(store blob.Store) *blockingFirstSlotStore {
	return &blockingFirstSlotStore{Store: store, blocked: make(chan struct{}), release: make(chan struct{})}
}

func (store *blockingFirstSlotStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if isSlot(key) && store.once.CompareAndSwap(false, true) {
		close(store.blocked)
		select {
		case <-store.release:
		case <-ctx.Done():
			return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: ctx.Err()}
		}
	}
	return store.Store.Create(ctx, key, data)
}

// peerWinsSlotStore lets a peer commit just before this store's first slot
// create, which then reports its outcome unknown.
type peerWinsSlotStore struct {
	blob.Store
	peer *Store
	done atomic.Bool
}

func (store *peerWinsSlotStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if isSlot(key) && store.done.CompareAndSwap(false, true) {
		if _, err := store.peer.Publish(ctx, backend.WriteRequest{Path: "/peer.md", ExpectedVersion: -1, Content: []byte("# Peer\n")}); err != nil {
			return blob.Attributes{}, err
		}
		return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: blob.ErrAmbiguous}
	}
	return store.Store.Create(ctx, key, data)
}

// unreachableSlotStore reports every slot create and read unavailable.
type unreachableSlotStore struct{ blob.Store }

func (store *unreachableSlotStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if isSlot(key) {
		return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: blob.ErrUnavailable}
	}
	return store.Store.Create(ctx, key, data)
}

func (store *unreachableSlotStore) Get(ctx context.Context, key string) (blob.Object, error) {
	if isSlot(key) {
		return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: blob.ErrUnavailable}
	}
	return store.Store.Get(ctx, key)
}

// blockingSlotCreateStore holds every slot create until its context ends.
type blockingSlotCreateStore struct{ blob.Store }

func (store *blockingSlotCreateStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if !isSlot(key) {
		return store.Store.Create(ctx, key, data)
	}
	<-ctx.Done()
	return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: errors.Join(blob.ErrAmbiguous, ctx.Err())}
}
