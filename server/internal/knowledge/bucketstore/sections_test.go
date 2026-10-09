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
	"github.com/latebit-io/demarkus/protocol/storefmt"
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
	reader := openSectionStore(t, gate, nil)
	reader.sections.wait = 20 * time.Millisecond

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
	store := openSectionStore(t, gate, nil)
	store.sections.wait = 20 * time.Millisecond
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
	reader := openSectionStore(t, slowGets{Store: memory, delay: 30 * time.Millisecond}, nil)
	reader.sections.idle, reader.sections.wait = time.Nanosecond, 5*time.Millisecond
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
	// The idle clock is the test's: it only moves when the test evicts, so the
	// GCs a heap sample costs can never let an eviction land between samples.
	store := openSectionStore(t, memory, nil)
	store.sections.idle, store.sections.wait = 50*time.Millisecond, time.Minute
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	store.now = func() time.Time { return time.Unix(0, clock.Load()) }
	const documents = 24
	bodyBytes := 0
	for n := range documents {
		body := memtest.AgentGraphBody(n)
		bodyBytes += len(body)
		write(t, store, fmt.Sprintf("/graph/%d.md", n), string(body), nil)
	}
	waitIdle(t, store)
	evicted := func() {
		clock.Add(int64(store.sections.idle))
		waitFor(t, "an evicted section index", func() bool { return !store.sections.isActive() })
	}
	build := func() {
		if rows := settledRows(t, store, "observations"); len(rows) != documents {
			t.Fatalf("body rows = %d, want %d", len(rows), documents)
		}
	}
	build()
	evicted()
	resident := memtest.Retained(build)
	if !store.sections.isActive() {
		t.Fatal("the index was evicted before the eviction sample; the idle clock moved on its own")
	}
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

// A checkpoint without segments builds the index from bodies, then writes
// the segments of its unchanged shards, from which the next replica builds
// without reading a body.
func TestCheckpointWithoutSegmentsBuildsFromBodies(t *testing.T) {
	memory := initializedMemory(t)
	writer := openSectionStore(t, memory, nil)
	write(t, writer, "/docs/a.md", "# A\n\n## Hairpin\n\nnat\n", nil)
	write(t, writer, "/docs/b.md", "# B\n\nkqueue swap\n", nil)
	mustSucceed(t, writer.checkpoint(context.Background()))
	for _, key := range listKeys(t, memory, segmentPrefix) {
		deleteObject(t, memory, key)
	}

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

// A shard's segment is written at most once a window: later checkpoints in
// the window leave it, and a reader takes what changed since from blobs. The
// next window's checkpoint carries unchanged bodies and reads only the rest.
func TestSegmentsAreWrittenOncePerWindow(t *testing.T) {
	ctx := context.Background()
	memory := initializedMemory(t)
	counted := newObservedBlobStore(memory)
	writer := openSectionStore(t, counted, nil)
	clock := time.Now()
	writer.now = func() time.Time { return clock }
	for n := range 5 {
		write(t, writer, fmt.Sprintf("/doc%d.md", n), fmt.Sprintf("# Doc %d\n\nbody %d\n", n, n), nil)
	}
	checkpointReads := func() int {
		counted.reset()
		mustSucceed(t, writer.checkpoint(ctx))
		return bodyReads(counted)
	}
	readerReads := func() int {
		reader := newObservedBlobStore(memory)
		assertRows(t, openSectionStore(t, reader, nil), "hairpin", "/doc2.md#doc-2")
		return bodyReads(reader)
	}
	if n := checkpointReads(); n != 5 {
		t.Errorf("first checkpoint read %d bodies, want all 5", n)
	}
	write(t, writer, "/doc2.md", "# Doc 2\n\nchanged hairpin\n", nil)
	if n := checkpointReads(); n != 0 {
		t.Errorf("checkpoint in the same window read %d bodies, want 0: it rewrote the segment", n)
	}
	if n := readerReads(); n != 1 {
		t.Errorf("reader read %d bodies, want the one changed since the segment", n)
	}
	clock = clock.Add(segmentWindow)
	write(t, writer, "/doc3.md", "# Doc 3\n\nbody 3\n", map[string]string{"tags": "renamed"})
	if n := checkpointReads(); n != 1 {
		t.Errorf("next window's checkpoint read %d bodies, want the 1 its last segment lacks", n)
	}
	if n := readerReads(); n != 0 {
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
	// Written an hour ago, so a superseded segment is past the grace.
	objects := clocked(memory)
	objects.writtenAgo(time.Hour)
	writer := openSectionStore(t, objects, nil)
	clock := time.Now()
	writer.now = func() time.Time { return clock }
	large := strings.Repeat("filler ", (900<<10)/7)
	for n := range 3 {
		write(t, writer, fmt.Sprintf("/big%d.md", n), fmt.Sprintf("# Big %d\n\nterm%d %s\n", n, n, large), nil)
	}
	// Escaped, every '<' takes six bytes: past any part.
	write(t, writer, "/escaped.md", "# Escaped\n\nhairpin "+strings.Repeat("<", protocol.MaxBodyLength-64)+"\n", nil)
	mustSucceed(t, writer.checkpoint(ctx))
	ref := newestSegment(t, memory, segmentLabel(0, writer.layout().Bits))
	keys := listKeys(t, memory, segmentDir(ref))
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
	// Once a later window's segment supersedes it, every part goes.
	clock = clock.Add(segmentWindow)
	write(t, writer, "/small.md", "# Small\n", nil)
	mustSucceed(t, writer.checkpoint(ctx))
	if left := listKeys(t, objects, segmentDir(ref)); len(left) != 0 {
		t.Errorf("parts of a superseded segment left: %v", left)
	}
}

// newestSegment is label's newest segment, which must exist.
func newestSegment(t *testing.T, objects blob.Store, label string) segmentRef {
	t.Helper()
	listing, err := listSegments(context.Background(), objects)
	mustSucceed(t, err)
	ref, ok := listing.latest(label)
	if !ok {
		t.Fatalf("no segment for %s", label)
	}
	return ref
}

// segmentDir is the prefix of ref's parts.
func segmentDir(ref segmentRef) string { return strings.TrimSuffix(segmentKey(ref, 0), "0.json") }

func listKeys(t *testing.T, objects blob.Store, prefix string) []string {
	t.Helper()
	var keys []string
	for page, err := range attributePages(context.Background(), objects, prefix, "") {
		mustSucceed(t, err)
		for _, attributes := range page {
			keys = append(keys, attributes.Key)
		}
	}
	return keys
}

// A checkpoint sweeps the segments a later window superseded and those of
// another shard layout once past the grace, and spares young ones.
func TestSupersededSegmentsAreSwept(t *testing.T) {
	ctx := context.Background()
	objects := newClockedStore(t)
	writer := openSectionStore(t, objects, nil)
	clock := time.Now()
	writer.now = func() time.Time { return clock }
	round := func(n int) segmentRef {
		write(t, writer, "/doc.md", fmt.Sprintf("# Doc\n\nround %d\n", n), nil)
		mustSucceed(t, writer.checkpoint(ctx))
		return newestSegment(t, objects, segmentLabel(0, writer.layout().Bits))
	}
	objects.writtenAgo(time.Hour)
	old := round(0)
	foreign := segmentRef{label: "07-00", window: old.window}
	mustSucceed(t, writeSegment(ctx, objects, foreign, []segmentBody{{Path: "/x.md", BodyHash: storefmt.ContentHash([]byte("x")), Body: "x"}}))
	// Stamped as the store's clock reads, a window on.
	objects.writtenAgo(-segmentWindow)
	clock = clock.Add(segmentWindow)
	young := round(1)
	clock = clock.Add(segmentWindow)
	newest := round(2)
	for _, gone := range []segmentRef{old, foreign} {
		if exists(t, objects, segmentKey(gone, 0)) {
			t.Errorf("segment %s window %d survived past the grace", gone.label, gone.window)
		}
	}
	for _, kept := range []segmentRef{young, newest} {
		if !exists(t, objects, segmentKey(kept, 0)) {
			t.Errorf("segment %s window %d was swept within the grace", kept.label, kept.window)
		}
	}
}

// Once writes stop, a section index still searched keeps one version: the
// older ones go once superseded for longer than a request can pin them.
func TestQuietSectionIndexKeepsOneVersion(t *testing.T) {
	store := openSectionStore(t, initializedMemory(t), func(options *Options) { options.RequestTimeout = 50 * time.Millisecond })
	store.sections.idle = 100 * time.Millisecond
	for round := range 4 {
		write(t, store, "/doc.md", fmt.Sprintf("# Doc\n\nround%d\n", round), nil)
		assertRows(t, store, fmt.Sprintf("round%d", round), "/doc.md#doc")
	}
	versions := func() int {
		store.sections.mu.Lock()
		defer store.sections.mu.Unlock()
		return len(store.sections.versions)
	}
	// Searching keeps the index from idle eviction while the worker idles.
	waitFor(t, "one section version", func() bool {
		assertRows(t, store, "round3", "/doc.md#doc")
		return versions() == 1
	})
}
