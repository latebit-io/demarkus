package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

const blobPrefix = objectPrefix + "blobs/"

// bodyReads is how many blob reads observed saw since its last reset.
func bodyReads(observed *observedBlobStore) int {
	return countPrefix(observed.counts().gets, blobPrefix)
}

// gatedGets holds blob reads while closed, counting those held.
type gatedGets struct {
	blob.Store
	mu   sync.Mutex
	open chan struct{}
	held atomic.Int64
}

func newGatedGets(objects blob.Store) *gatedGets {
	gate := &gatedGets{Store: objects, open: make(chan struct{})}
	close(gate.open)
	return gate
}

func (s *gatedGets) hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = make(chan struct{})
}

func (s *gatedGets) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.open)
}

func (s *gatedGets) Get(ctx context.Context, key string) (blob.Object, error) {
	if strings.HasPrefix(key, blobPrefix) {
		s.mu.Lock()
		open := s.open
		s.mu.Unlock()
		s.held.Add(1)
		defer s.held.Add(-1)
		select {
		case <-open:
		case <-ctx.Done():
			return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: ctx.Err()}
		}
	}
	return s.Store.Get(ctx, key)
}

// openSectionStore opens a store with the compactor manual; configure may
// change its options.
func openSectionStore(t *testing.T, objects blob.Store, configure func(*Options)) *Store {
	t.Helper()
	options := Options{Logger: discardLogger, WorldID: testWorldID, trigger: manual, noHedge: true}
	if configure != nil {
		configure(&options)
	}
	store, err := Open(context.Background(), objects, options)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeAtEnd(t, store)
	return store
}

func bodyOptions() catalog.Options { return catalog.Options{Match: catalog.MatchBody} }

// bodyRows runs one body lookup and returns its path#anchor rows, sorted.
func bodyRows(store *Store, query string) ([]string, error) {
	results, err := store.Lookup(query, bodyOptions())
	return locations(results), err
}

func locations(results []catalog.Result) []string {
	rows := make([]string, len(results))
	for index := range results {
		rows[index] = results[index].Location()
	}
	sort.Strings(rows)
	return rows
}

// settledRows is bodyRows once the store answers from sections.
func settledRows(t *testing.T, store *Store, query string) []string {
	t.Helper()
	results, err := storetest.SettledLookup(store.direct(), query, bodyOptions())
	if err != nil {
		t.Fatalf("body lookup %q: %v", query, err)
	}
	return locations(results)
}

func assertRows(t *testing.T, store *Store, query string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := settledRows(t, store, query); !slices.Equal(got, want) {
		t.Errorf("body lookup %q = %v, want %v", query, got, want)
	}
}

func write(t *testing.T, store *Store, path, body string, meta map[string]string) {
	t.Helper()
	_, err := store.WriteVersion(path, -1, []byte(body), meta)
	mustSucceed(t, err)
}

func (index *sectionIndex) isActive() bool {
	index.mu.Lock()
	defer index.mu.Unlock()
	return index.active
}

// Writes, slots applied by a replica and catalog lookups do no section work:
// no store reads a body until the first body search.
func TestNoSectionWorkBeforeBodySearch(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	counted := newObservedBlobStore(memory)
	reader := openSectionStore(t, counted, nil)
	write(t, writer, "/docs/a.md", "# A\n\n## Hairpin\n\nnat on the same host\n", map[string]string{"tags": "net"})
	write(t, writer, "/docs/b.md", "# B\n\nkqueue symlink swap\n", nil)
	write(t, writer, "/docs/b.md", "# B\n\n## Poison\n\nlock pid write\n", nil)
	_, _, err := writer.ArchiveResult("/docs/b.md", true)
	mustSucceed(t, err)
	mustSucceed(t, writer.checkpoint(ctx))
	mustSucceed(t, reader.poll(ctx))
	if _, err := reader.Lookup("net", catalog.Options{}); err != nil {
		t.Fatalf("catalog lookup: %v", err)
	}
	if n := bodyReads(counted); n != 0 {
		t.Errorf("replica read %d bodies before any body search", n)
	}
	if writer.sections.isActive() || reader.sections.isActive() {
		t.Error("a section index started before any body search")
	}
	assertRows(t, reader, "hairpin", "/docs/a.md#hairpin")
	assertRows(t, reader, "poison")
	if writer.sections.isActive() {
		t.Error("the writer started a section index for the replica's body search")
	}
}

// The first body search on a world whose index is still building answers as
// catalog fallback, through the store and through the handler; it answers
// from sections once the build finishes.
func TestFirstBodySearchFallsBack(t *testing.T) {
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	write(t, writer, "/docs/a.md", "# A\n\n## Hairpin\n\nnat on the same host\n", map[string]string{"tags": "net"})
	gate := newGatedGets(memory)
	gate.hold()
	reader := openSectionStore(t, gate, func(options *Options) { options.bodyWait = 20 * time.Millisecond })

	if _, err := bodyRows(reader, "hairpin"); !errors.Is(err, backend.ErrBodyMatchUnavailable) {
		t.Fatalf("first body search = %v, want ErrBodyMatchUnavailable", err)
	}
	h := storetest.NewHandler(storetest.LookupBackend{Store: reader})
	resp := storetest.Send(t, h, protocol.Request{Verb: protocol.VerbLookup, Path: "/", Metadata: map[string]string{"query": "net", "match": "body"}})
	if resp.Metadata["match"] != "catalog" || resp.Metadata["matches"] != "1" || !strings.Contains(resp.Body, "| /docs/a.md |") {
		t.Errorf("handler answer: match %q matches %q, want the catalog answer:\n%s", resp.Metadata["match"], resp.Metadata["matches"], resp.Body)
	}
	gate.release()
	assertRows(t, reader, "hairpin", "/docs/a.md#hairpin")
}

// Once caught up, body search on the writer and on a replica answers what an
// index built eagerly from the same documents does, across new documents,
// body and metadata changes, appends, archive and unarchive.
func TestBodySearchMatchesEagerIndex(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	reader := openSectionStore(t, memory, nil)
	queries := []string{"hairpin", "kqueue", "poison", "lock", "net", "nat host", "renamed", "appended", "gotchas", "sysctl", "r5", "stable r5", "r4"}
	round := func(n int) {
		write(t, writer, fmt.Sprintf("/docs/%d.md", n), fmt.Sprintf("# Doc %d\n\n## Hairpin\n\nnat on host %d\n", n, n), map[string]string{"tags": "net"})
		write(t, writer, "/docs/b.md", fmt.Sprintf("# B\n\n## Poison %d\n\nlock pid write\n", n), nil)
		write(t, writer, "/docs/c.md", "# C\n\nkqueue symlink swap\n", map[string]string{"title": fmt.Sprintf("Renamed %d", n)})
		write(t, writer, "/docs/meta.md", "# Meta\n\nstable body\n", map[string]string{"tags": fmt.Sprintf("r%d", n)})
		current, err := writer.CurrentVersionResult("/docs/c.md")
		mustSucceed(t, err)
		_, err = writer.AppendVersion("/docs/c.md", current, []byte("\n## Appended\n\nsysctl gotchas\n"), nil)
		mustSucceed(t, err)
		_, _, err = writer.ArchiveResult(fmt.Sprintf("/docs/%d.md", n-1), n%2 == 0)
		if n > 0 {
			mustSucceed(t, err)
		}
	}
	round(0)
	for _, store := range []*Store{writer, reader} {
		settledRows(t, store, "hairpin")
	}
	for n := 1; n < 6; n++ {
		round(n)
		if n == 3 {
			mustSucceed(t, writer.checkpoint(ctx))
		}
	}
	mustSucceed(t, reader.poll(ctx))
	eager := catalog.New()
	writer.served.Load().snap.Paths.Ascend(func(state *pathState) bool {
		if !state.Archived {
			document, err := writer.Get(state.Path, 0)
			mustSucceed(t, err)
			eager.Put(state.Path, document.Metadata, document.Content, state.Modified)
		}
		return true
	})
	for _, query := range queries {
		want, err := eager.Lookup(query, bodyOptions())
		mustSucceed(t, err)
		for name, store := range map[string]*Store{"writer": writer, "replica": reader} {
			if got := settledRows(t, store, query); !slices.Equal(got, locations(want)) {
				t.Errorf("%s body lookup %q = %v, eager index %v", name, query, got, locations(want))
			}
		}
	}
}

// A body search whose snapshot the index has not caught up to answers as
// catalog fallback, never from sections that miss part of it.
func TestLaggingSectionIndexFallsBack(t *testing.T) {
	memory := initializedMemory(t)
	gate := newGatedGets(memory)
	store := openSectionStore(t, gate, func(options *Options) { options.bodyWait = 20 * time.Millisecond })
	write(t, store, "/a.md", "# A\n\nhairpin nat\n", nil)
	assertRows(t, store, "hairpin", "/a.md#a")

	gate.hold()
	write(t, store, "/b.md", "# B\n\nkqueue swap\n", nil)
	for _, query := range []string{"kqueue", "hairpin"} {
		if rows, err := bodyRows(store, query); !errors.Is(err, backend.ErrBodyMatchUnavailable) {
			t.Errorf("body lookup %q behind the index = %v, %v; want ErrBodyMatchUnavailable", query, rows, err)
		}
	}
	gate.release()
	assertRows(t, store, "kqueue", "/b.md#b")
}

// A replica that catches up an archive and an unarchive in one pass leaves
// the document searchable: the unarchive reads its body again.
func TestArchiveAndUnarchiveInOneCatchUp(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	gate := newGatedGets(memory)
	reader := openSectionStore(t, gate, nil)
	write(t, writer, "/a.md", "# A\n\nhairpin nat\n", nil)
	mustSucceed(t, reader.poll(ctx))
	assertRows(t, reader, "hairpin", "/a.md#a")
	gate.hold()
	write(t, writer, "/b.md", "# B\n\nkqueue swap\n", nil)
	mustSucceed(t, reader.poll(ctx))
	waitFor(t, "the index reading b", func() bool { return gate.held.Load() > 0 })
	// One install each, queued behind b.
	for _, archived := range []bool{true, false} {
		_, _, err := writer.ArchiveResult("/a.md", archived)
		mustSucceed(t, err)
		mustSucceed(t, reader.poll(ctx))
	}
	gate.release()
	assertRows(t, reader, "hairpin", "/a.md#a")
}

// Installs past the queue's bound while the index is stuck are caught up
// in one comparison with the served snapshot.
func TestSectionQueueOverflowResyncs(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	gate := newGatedGets(memory)
	reader := openSectionStore(t, gate, nil)
	write(t, writer, "/a.md", "# A\n\nhairpin nat\n", nil)
	mustSucceed(t, reader.poll(ctx))
	assertRows(t, reader, "hairpin", "/a.md#a")
	gate.hold()
	write(t, writer, "/b.md", "# B\n\nkqueue swap\n", nil)
	mustSucceed(t, reader.poll(ctx))
	waitFor(t, "the index reading b", func() bool { return gate.held.Load() > 0 })
	for n := range sectionSteps + 8 {
		write(t, writer, fmt.Sprintf("/many/%d.md", n), fmt.Sprintf("# Many %d\n\nterm%d\n", n, n), nil)
		mustSucceed(t, reader.poll(ctx))
	}
	_, _, err := writer.ArchiveResult("/a.md", true)
	mustSucceed(t, err)
	mustSucceed(t, reader.poll(ctx))
	gate.release()
	assertRows(t, reader, fmt.Sprintf("term%d", sectionSteps+7), fmt.Sprintf("/many/%d.md#many-%d", sectionSteps+7, sectionSteps+7))
	assertRows(t, reader, "term3", "/many/3.md#many-3")
	assertRows(t, reader, "hairpin")
}

// A view pinned before a change answers body search from its own snapshot,
// though the index has moved past it.
func TestBodySearchAnswersThePinnedSnapshot(t *testing.T) {
	ctx := context.Background()
	store := openSectionStore(t, initializedMemory(t), nil)
	write(t, store, "/a.md", "# A\n\nalpha one\n", nil)
	assertRows(t, store, "alpha", "/a.md#a")
	view, err := store.OpenReadView(ctx)
	mustSucceed(t, err)
	defer func() { mustSucceed(t, view.Close()) }()
	_, err = view.IsDir(ctx, "/")
	mustSucceed(t, err)
	write(t, store, "/a.md", "# A\n\nbeta two\n", nil)
	assertRows(t, store, "beta", "/a.md#a")
	for query, want := range map[string]int{"alpha": 1, "beta": 0} {
		results, err := view.Lookup(ctx, query, bodyOptions())
		if err != nil || len(results) != want {
			t.Errorf("pinned view body lookup %q = %d rows, %v; want %d", query, len(results), err, want)
		}
	}
}

// slowGets delays every blob read.
type slowGets struct {
	blob.Store
	delay time.Duration
}

func (s slowGets) Get(ctx context.Context, key string) (blob.Object, error) {
	if strings.HasPrefix(key, blobPrefix) {
		time.Sleep(s.delay)
	}
	return s.Store.Get(ctx, key)
}

// A build slower than a body search's wait is not evicted while searches
// keep retrying, however short the idle period.
func TestSlowBuildIsKeptWhileSearched(t *testing.T) {
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	for n := range 3 {
		write(t, writer, fmt.Sprintf("/doc%d.md", n), fmt.Sprintf("# Doc %d\n\nhairpin %d\n", n, n), nil)
	}
	reader := openSectionStore(t, slowGets{Store: memory, delay: 30 * time.Millisecond}, func(options *Options) {
		options.sectionIdle, options.bodyWait = time.Nanosecond, 5*time.Millisecond
	})
	if rows := settledRows(t, reader, "hairpin"); len(rows) != 3 {
		t.Errorf("body rows = %v, want 3", rows)
	}
}

// An idle world drops its section index, and with it the heap the index
// held; the next body search builds it again.
func TestIdleSectionIndexReleasesMemory(t *testing.T) {
	memory, err := blob.NewMemory(4 << 20)
	mustSucceed(t, err)
	mustSucceed(t, initialize(context.Background(), memory, testWorldID))
	// A search waits out the build, which the race detector slows past a second.
	store := openSectionStore(t, memory, func(options *Options) {
		options.sectionIdle, options.bodyWait = 50*time.Millisecond, time.Minute
	})
	const documents = 24
	bodyBytes := 0
	for n := range documents {
		body := memtest.AgentGraphBody(n)
		bodyBytes += len(body)
		write(t, store, fmt.Sprintf("/graph/%d.md", n), string(body), nil)
	}
	waitIdle(t, store)
	evicted := func() { waitFor(t, "an evicted section index", func() bool { return !store.sections.isActive() }) }
	build := func() {
		if rows := settledRows(t, store, "observations"); len(rows) != documents {
			t.Fatalf("body rows = %d, want %d", len(rows), documents)
		}
	}
	build()
	evicted()
	resident := memtest.Retained(build)
	released := memtest.Retained(evicted)
	t.Logf("resident index %d bytes for %d body bytes (%.2f per byte); eviction released %d", resident, bodyBytes, float64(resident)/float64(bodyBytes), -released)
	if resident < int64(bodyBytes)/2 {
		t.Errorf("resident index %d bytes for %d body bytes: the build measured nothing", resident, bodyBytes)
	}
	if left := resident + released; left > 512<<10 {
		t.Errorf("an evicted index left %d bytes of %d", left, resident)
	}
	growth := memtest.Retained(func() {
		for range 5 {
			build()
			evicted()
		}
	})
	if growth > 512<<10 {
		t.Errorf("five build and evict cycles grew heap %d bytes", growth)
	}
}

// An active index under the agent's publish-and-prune loop tracks the live
// documents: versions past a request's lifetime are dropped as new ones come.
func TestActiveSectionIndexTracksLiveData(t *testing.T) {
	memory, err := blob.NewMemory(4 << 20)
	mustSucceed(t, err)
	objects := &storedBytes{Store: memory}
	mustSucceed(t, initialize(context.Background(), objects, testWorldID))
	const requestTimeout = 200 * time.Millisecond
	store := openSectionStore(t, objects, func(options *Options) { options.RequestTimeout = requestTimeout })
	cycle := func(n int) {
		publishPruned(t, store, "/graph.md", memtest.AgentGraphBody(n))
		publishPruned(t, store, "/index.md", fmt.Appendf(nil, "# Index\n\nCycle %d observations.\n", n))
		settledRows(t, store, "observations")
	}
	settle := func() {
		time.Sleep(requestTimeout + 50*time.Millisecond)
		cycle(-1)
	}
	const warmup, cycles = 25, 60
	for n := range warmup {
		cycle(n)
	}
	settle()
	bytesBefore, objectsBefore := objects.bytes.Load(), objects.objects.Load()
	growth := memtest.Retained(func() {
		for n := warmup; n < warmup+cycles; n++ {
			cycle(n)
		}
		settle()
	})
	bucket := objects.bytes.Load() - bytesBefore + 512*(objects.objects.Load()-objectsBefore)
	bodyBytes := int64(len(memtest.AgentGraphBody(0)))
	t.Logf("heap grew %d bytes, %d of them the bucket's", growth, bucket)
	if limit := 2 * bodyBytes; growth-bucket > limit {
		t.Errorf("heap outside the bucket grew %d bytes over %d cycles, want under %d", growth-bucket, cycles, limit)
	}
}

// A checkpoint without segments, schema 1 or folded, builds the index from
// bodies, then writes the segments of its unchanged shards, from which the
// next replica builds without reading a body.
func TestCheckpointWithoutSegmentsBuildsFromBodies(t *testing.T) {
	tests := []struct {
		name  string
		world func(t *testing.T) *blob.Memory
	}{
		{name: "schema 1", world: func(t *testing.T) *blob.Memory {
			memory := initializedMemory(t)
			commitReadDocuments(t, memory, []readDocumentSpec{
				newReadDocument("/docs/a.md", "# A v1\n", "# A\n\n## Hairpin\n\nnat\n"),
				newReadDocument("/docs/b.md", "# B\n\nkqueue swap\n"),
			})
			return memory
		}},
		{name: "folded", world: func(t *testing.T) *blob.Memory {
			memory := initializedMemory(t)
			writer := openSectionStore(t, memory, nil)
			write(t, writer, "/docs/a.md", "# A\n\n## Hairpin\n\nnat\n", nil)
			write(t, writer, "/docs/b.md", "# B\n\nkqueue swap\n", nil)
			mustSucceed(t, writer.checkpoint(context.Background()))
			for _, key := range listKeys(t, memory, segmentPrefix) {
				deleteObject(t, memory, key)
			}
			return memory
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := tt.world(t)
			first := newObservedBlobStore(memory)
			assertRows(t, openSectionStore(t, first, nil), "hairpin", "/docs/a.md#hairpin")
			if bodyReads(first) == 0 {
				t.Error("built without reading a body")
			}
			if len(listKeys(t, memory, segmentPrefix)) == 0 {
				t.Error("no segment written for the unchanged shards")
			}
			second := newObservedBlobStore(memory)
			assertRows(t, openSectionStore(t, second, nil), "kqueue", "/docs/b.md#b")
			if n := bodyReads(second); n != 0 {
				t.Errorf("replica after the repair read %d bodies, want the segments only", n)
			}
		})
	}
}

// A segment that fails its checks is reported and its bodies read one by one;
// the builder does not write over it.
func TestCorruptSegmentFallsBackToBodies(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	write(t, writer, "/a.md", "# A\n\nhairpin nat\n", nil)
	mustSucceed(t, writer.checkpoint(ctx))
	keys := listKeys(t, memory, segmentPrefix)
	if len(keys) != 1 {
		t.Fatalf("segments = %v, want one part", keys)
	}
	part := getObject(t, memory, keys[0])
	var segment segmentObject
	decodeObject(t, part.Data, &segment)
	segment.Bodies[0].Body = "# A\n\ntampered\n"
	data, err := marshalImmutable(segment)
	mustSucceed(t, err)
	_, err = memory.Replace(ctx, keys[0], part.Attributes.Generation, data)
	mustSucceed(t, err)
	counted := newObservedBlobStore(memory)
	assertRows(t, openSectionStore(t, counted, nil), "hairpin", "/a.md#a")
	if bodyReads(counted) == 0 {
		t.Error("built from a segment whose body does not hash to its entry")
	}
	if !slices.Equal(getObject(t, memory, keys[0]).Data, data) {
		t.Error("the builder wrote over a corrupt segment")
	}
}

// A compactor carries unchanged bodies from the previous checkpoint's
// segment and reads only those that changed; a reader then builds from the
// segments alone.
func TestSegmentsCarryUnchangedBodies(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	counted := newObservedBlobStore(memory)
	writer := openSectionStore(t, counted, nil)
	for n := range 5 {
		write(t, writer, fmt.Sprintf("/doc%d.md", n), fmt.Sprintf("# Doc %d\n\nbody %d\n", n, n), nil)
	}
	checkpointReads := func() int {
		counted.reset()
		mustSucceed(t, writer.checkpoint(ctx))
		return bodyReads(counted)
	}
	if n := checkpointReads(); n != 5 {
		t.Errorf("first checkpoint read %d bodies, want all 5", n)
	}
	write(t, writer, "/doc2.md", "# Doc 2\n\nchanged hairpin\n", nil)
	if n := checkpointReads(); n != 1 {
		t.Errorf("checkpoint after one body changed read %d bodies, want 1", n)
	}
	write(t, writer, "/doc3.md", "# Doc 3\n\nbody 3\n", map[string]string{"tags": "renamed"})
	if n := checkpointReads(); n != 0 {
		t.Errorf("checkpoint after a metadata change read %d bodies, want 0", n)
	}
	layout := writer.layout()
	bodies, err := readSegment(ctx, memory, layout.Shards[0])
	if err != nil || len(bodies) != 5 {
		t.Fatalf("segment of the newest shard: %d bodies, %v; want 5", len(bodies), err)
	}
	reader := newObservedBlobStore(memory)
	assertRows(t, openSectionStore(t, reader, nil), "hairpin", "/doc2.md#doc-2")
	if n := bodyReads(reader); n != 0 {
		t.Errorf("reader read %d bodies, want the segment only", n)
	}
}

// A shard's bodies split into parts under the part limit, and a body too
// large for any part is left to its blob; a reader builds from both.
func TestSegmentParts(t *testing.T) {
	ctx := context.Background()
	memory, err := blob.NewMemory(4 << 20)
	mustSucceed(t, err)
	mustSucceed(t, initialize(ctx, memory, testWorldID))
	writer := openSectionStore(t, memory, nil)
	large := strings.Repeat("filler ", (900<<10)/7)
	for n := range 3 {
		write(t, writer, fmt.Sprintf("/big%d.md", n), fmt.Sprintf("# Big %d\n\nterm%d %s\n", n, n, large), nil)
	}
	// Escaped, every '<' takes six bytes: past any part.
	write(t, writer, "/escaped.md", "# Escaped\n\nhairpin "+strings.Repeat("<", protocol.MaxBodyLength-64)+"\n", nil)
	mustSucceed(t, writer.checkpoint(ctx))
	ref := writer.layout().Shards[0]
	keys := listKeys(t, memory, segmentPrefix+ref.Shard+"/"+ref.Hash+"/")
	if len(keys) != 2 {
		t.Fatalf("segment parts = %v, want 2", keys)
	}
	bodies, err := readSegment(ctx, memory, ref)
	if err != nil || len(bodies) != 3 {
		t.Fatalf("segment holds %d bodies, %v; want the three that fit", len(bodies), err)
	}
	counted := newObservedBlobStore(memory)
	reader := openSectionStore(t, counted, nil)
	assertRows(t, reader, "term2", "/big2.md#big-2")
	assertRows(t, reader, "hairpin", "/escaped.md#escaped")
	if n := bodyReads(counted); n != 1 {
		t.Errorf("reader read %d bodies, want only the one no part holds", n)
	}
	// Once its checkpoint is dropped, every part of its shard's segment goes.
	for n := range checkpointsKept + 1 {
		write(t, writer, "/small.md", fmt.Sprintf("# Small %d\n", n), nil)
		mustSucceed(t, writer.checkpoint(ctx))
	}
	if left := listKeys(t, memory, segmentPrefix+ref.Shard+"/"+ref.Hash+"/"); len(left) != 0 {
		t.Errorf("parts of a dropped checkpoint's segment left: %v", left)
	}
}

func listKeys(t *testing.T, objects blob.Store, prefix string) []string {
	t.Helper()
	var keys []string
	cursor := ""
	for {
		page, err := objects.List(context.Background(), prefix, "", cursor)
		mustSucceed(t, err)
		for _, attributes := range page.Objects {
			keys = append(keys, attributes.Key)
		}
		if page.NextCursor == "" {
			return keys
		}
		cursor = page.NextCursor
	}
}

// A drop sweeps every segment part whose shard no kept checkpoint uses once it
// is past the grace, orphans included, and spares young ones and kept shards'.
func TestDropSweepsUnusedSegments(t *testing.T) {
	ctx := context.Background()
	objects := newClockedStore(t)
	writer := openSectionStore(t, objects, nil)
	orphan := func(name string) string {
		key := segmentKey(shardRef{Shard: "0", objectRef: objectRef{Hash: hashHex([]byte(name))}}, 3)
		_, err := objects.Create(ctx, key, []byte("{}"))
		mustSucceed(t, err)
		return key
	}
	objects.writtenAgo(3 * time.Hour)
	old := orphan("old")
	for n := range checkpointsKept + 1 {
		write(t, writer, "/doc.md", fmt.Sprintf("# Doc\n\nround %d\n", n), nil)
		mustSucceed(t, writer.checkpoint(ctx))
	}
	objects.writtenAgo(0)
	young := orphan("young")
	write(t, writer, "/doc.md", "# Doc\n\nlast round\n", nil)
	mustSucceed(t, writer.checkpoint(ctx))
	if exists(t, objects, old) {
		t.Error("an old orphan segment part survived the drop")
	}
	if !exists(t, objects, young) {
		t.Error("a young segment part was swept within the grace")
	}
	if !exists(t, objects, segmentKey(writer.layout().Shards[0], 0)) {
		t.Error("the newest checkpoint's segment was swept")
	}
}
