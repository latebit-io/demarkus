package federation

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/graphstore"
	"golang.org/x/time/rate"
)

// harness runs a deriver over one fake world, alpha, and a fake hub.
type harness struct {
	t      *testing.T
	worlds fakeSource
	alpha  *fakeWorld
	hub    *fakeHub
}

func newHarness(t *testing.T) *harness {
	alpha := newFakeWorld(t)
	return &harness{t: t, worlds: fakeSource{"alpha": alpha}, alpha: alpha, hub: newFakeHub()}
}

// seededRows are the rows seedAlpha leaves public.
var seededRows = map[string]int{"/index.md": 1, "/a.md": 1, "/docs/c.md": 1}

// seeded is a harness whose alpha holds public, private, archived and
// generated documents, already checkpointed by a running deriver.
func seeded(t *testing.T) (h *harness, stop func()) {
	h = newHarness(t)
	alpha := h.alpha
	alpha.publish("/index.md", "# Home\n\n[a](a.md) [c](docs/c.md) [beta](mark://beta/x.md) [web](https://example.com/)\n")
	alpha.publish("/a.md", "# A\n\n[home](/index.md)\n")
	alpha.publish("/docs/c.md", "# C\n")
	alpha.publish("/secret.md", "# Secret\n\n[a](a.md)\n")
	alpha.docs["/secret.md"].private = true
	alpha.publish("/old.md", "# Old\n")
	alpha.docs["/old.md"].archived = true
	alpha.publish(graphstore.WorldShardPath("alpha", "0"), "# generated\n")
	stop = h.start()
	h.await(versions(seededRows))
	return h, stop
}

// with is seededRows plus more.
func with(more map[string]int) map[string]int {
	rows := maps.Clone(seededRows)
	maps.Copy(rows, more)
	return rows
}

// start runs a deriver until the returned stop, which waits for it to end.
func (h *harness) start() (stop func()) {
	d := newDeriver(Config{
		Worlds: slices.Collect(maps.Keys(h.worlds)), Source: h.worlds, Hub: h.hub.io(),
		QuietPeriod: 20 * time.Millisecond, Interval: 50 * time.Millisecond,
		Log: slog.New(slog.DiscardHandler),
	})
	d.reads = rate.NewLimiter(rate.Inf, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	h.t.Cleanup(stop)
	return stop
}

// checkpoint loads alpha's checkpoint from the hub as a reader would.
func (h *harness) checkpoint() (graphstore.WorldManifest, map[string]graphstore.WorldSource, error) {
	ctx := context.Background()
	head, err := h.hub.fetch(ctx, graphstore.WorldManifestPath("alpha"))
	if err != nil {
		return graphstore.WorldManifest{}, nil, err
	}
	load, err := graphstore.LoadWorld(ctx, graphstore.WorldLoadRequest{World: "alpha", Manifest: head, Fetch: h.hub.fetch})
	rows := map[string]graphstore.WorldSource{}
	for _, source := range load.Sources {
		rows[source.Path] = source
	}
	return load.Manifest, rows, err
}

// await polls until alpha's checkpoint satisfies check.
func (h *harness) await(check func(graphstore.WorldManifest, map[string]graphstore.WorldSource) error) graphstore.WorldManifest {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, rows, err := h.checkpoint()
		if err == nil {
			err = check(m, rows)
		}
		if err == nil {
			return m
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("checkpoint of alpha: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rewriteManifest publishes alpha's manifest as edit leaves it, the way
// another writer would.
func (h *harness) rewriteManifest(edit func(*graphstore.WorldManifest)) {
	h.t.Helper()
	ctx, path := context.Background(), graphstore.WorldManifestPath("alpha")
	head, err := h.hub.fetch(ctx, path)
	if err != nil {
		h.t.Fatal(err)
	}
	m, err := graphstore.ParseWorldManifest("alpha", head.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	edit(&m)
	body, err := graphstore.BuildWorldManifest(m)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.hub.publish(ctx, path, body, -1); err != nil {
		h.t.Fatal(err)
	}
}

// versions checks a complete checkpoint holds exactly the rows at these versions.
func versions(want map[string]int) func(graphstore.WorldManifest, map[string]graphstore.WorldSource) error {
	return func(m graphstore.WorldManifest, rows map[string]graphstore.WorldSource) error {
		got := map[string]int{}
		for p, row := range rows {
			got[p] = row.Version
		}
		if !m.Complete || !maps.Equal(got, want) {
			return fmt.Errorf("complete %t, rows %v, want %v", m.Complete, got, want)
		}
		return nil
	}
}

// quiet lets several checkpoint intervals pass.
func quiet() { time.Sleep(300 * time.Millisecond) }

func TestDeriverCheckpointsTheWorldsPublicRows(t *testing.T) {
	h, _ := seeded(t)
	_, rows, err := h.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	index := rows["/index.md"]
	targets := make([]string, 0, len(index.Edges))
	for _, edge := range index.Edges {
		targets = append(targets, edge.To)
	}
	if want := []string{"mark://alpha/a.md", "mark://alpha/docs/c.md", "mark://beta/x.md"}; !slices.Equal(targets, want) || index.Title != "Home" {
		t.Errorf("index row = %+v, want title Home and edges to %v", index, want)
	}
	for _, p := range []string{"/secret.md", "/old.md", graphstore.WorldShardPath("alpha", "0")} {
		if reads := h.alpha.reads(p); reads != 0 {
			t.Errorf("%s read %d times, want never", p, reads)
		}
	}
}

func TestDeriverWritesOnlyWhatChanged(t *testing.T) {
	h, _ := seeded(t)
	m, _, err := h.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	mark := h.hub.mark()
	h.alpha.publish("/a.md", "# A\n\n[c](docs/c.md)\n")
	h.await(versions(with(map[string]int{"/a.md": 2})))
	quiet()
	shard := graphstore.WorldShardPath("alpha", graphstore.SourcePrefix("/a.md", m.PrefixLength))
	if got, want := h.hub.written(mark), []string{shard, graphstore.WorldManifestPath("alpha")}; !slices.Equal(got, want) {
		t.Errorf("one edit wrote %v, want %v", got, want)
	}

	h.alpha.archive("/docs/c.md")
	h.await(versions(map[string]int{"/index.md": 1, "/a.md": 2}))

	// Idle: no reads, no writes.
	mark, before := h.hub.mark(), h.alpha.reads("/index.md")
	quiet()
	if got := h.hub.written(mark); len(got) != 0 {
		t.Errorf("an idle world wrote %v", got)
	}
	if h.alpha.reads("/index.md") != before {
		t.Errorf("an idle world was read")
	}
}

func TestDeriverResumesFromItsCheckpoint(t *testing.T) {
	h, stop := seeded(t)
	stop()
	h.alpha.publish("/a.md", "# A\n")
	h.alpha.publish("/b.md", "# B\n")
	reads, lists := h.alpha.reads("/index.md"), h.alpha.listCount()
	h.start()
	h.await(versions(with(map[string]int{"/a.md": 2, "/b.md": 1})))
	if h.alpha.reads("/index.md") != reads || h.alpha.listCount() != lists {
		t.Error("resume reread the world")
	}
}

// A checkpoint whose cursor is behind its rows, as a rebuild or an older
// leader's leaves it, replays changes the rows already hold without a read.
func TestDeriverReplayIsGuardedByVersion(t *testing.T) {
	h, stop := seeded(t)
	stop()
	h.rewriteManifest(func(m *graphstore.WorldManifest) { m.Cursor.Seq = 0 }) // before every change
	reads := map[string]int{}
	for _, p := range []string{"/index.md", "/a.md", "/docs/c.md", graphstore.WorldShardPath("alpha", "0")} {
		reads[p] = h.alpha.reads(p)
	}
	h.alpha.publish("/b.md", "# B\n")
	h.start()
	h.await(versions(with(map[string]int{"/b.md": 1})))
	for p, before := range reads {
		if after := h.alpha.reads(p); after != before {
			t.Errorf("%s reread on replay: %d -> %d", p, before, after)
		}
	}
}

// A resync rebuilds the world, but rewrites only the shards that changed.
func TestDeriverRebuildsAfterAResync(t *testing.T) {
	h, _ := seeded(t)
	lists, mark := h.alpha.listCount(), h.hub.mark()
	h.alpha.newEpoch()
	h.alpha.publish("/b.md", "# B\n")
	m := h.await(versions(with(map[string]int{"/b.md": 1})))
	if m.Cursor.Epoch != "e2" {
		t.Errorf("cursor = %v, want the new epoch", m.Cursor)
	}
	if h.alpha.listCount() == lists {
		t.Error("the world was not read again")
	}
	quiet()
	shard := graphstore.WorldShardPath("alpha", graphstore.SourcePrefix("/b.md", m.PrefixLength))
	for _, p := range h.hub.written(mark) {
		if p != shard && p != graphstore.WorldManifestPath("alpha") {
			t.Errorf("the rebuild rewrote unchanged %s", p)
		}
	}
}

func TestDeriverHoldsTheCheckpointIncompleteUntilAFailedReadSucceeds(t *testing.T) {
	h, _ := seeded(t)
	h.alpha.setFailing("/a.md", true)
	h.alpha.publish("/a.md", "# A\n")
	h.alpha.publish("/b.md", "# B\n")
	h.await(func(m graphstore.WorldManifest, rows map[string]graphstore.WorldSource) error {
		if m.Complete || rows["/b.md"].Version != 1 {
			return fmt.Errorf("complete %t with rows %v, want incomplete with /b.md", m.Complete, rows)
		}
		return nil
	})
	h.alpha.setFailing("/a.md", false)
	h.await(versions(with(map[string]int{"/a.md": 2, "/b.md": 1})))
}

func TestDeriverRecoversWhenAnotherWriterMovedTheManifest(t *testing.T) {
	h, _ := seeded(t)
	// A deposed leader's late write: a valid manifest from an older cursor.
	h.rewriteManifest(func(m *graphstore.WorldManifest) { m.Cursor.Seq-- })
	h.alpha.publish("/b.md", "# B\n")
	h.await(versions(with(map[string]int{"/b.md": 1})))
}

// A world found empty after a resync still owes the manifest, which must
// drop every shard it pinned.
func TestDeriverEmptiesTheCheckpointOfAWorldFoundEmpty(t *testing.T) {
	h, _ := seeded(t)
	h.alpha.mu.Lock()
	clear(h.alpha.docs)
	h.alpha.mu.Unlock()
	h.alpha.newEpoch()
	if m := h.await(versions(map[string]int{})); len(m.Shards) != 0 || m.Cursor.Epoch != "e2" {
		t.Errorf("manifest = %+v, want no shards in the new epoch", m)
	}
}

func TestDeriverRetriesAFailedHubWrite(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/a.md", "# A\n")
	h.hub.setFailing(graphstore.WorldManifestPath("alpha"), true)
	h.start()
	quiet()
	if _, _, err := h.checkpoint(); err == nil {
		t.Fatal("a manifest was written while the hub refused it")
	}
	h.hub.setFailing(graphstore.WorldManifestPath("alpha"), false)
	h.await(versions(map[string]int{"/a.md": 1}))
}

// A shard past its target splits the world one hex digit further; the old
// shards are rewritten empty, so no stale rows stay behind them.
func TestDeriverSplitsOutgrownShards(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/a.md", "# A\n")
	h.start()
	first := h.await(versions(map[string]int{"/a.md": 1}))
	if first.PrefixLength != 1 {
		t.Fatalf("prefix length = %d, want 1", first.PrefixLength)
	}

	// Three sources of a thousand edges overfill one shard.
	want := map[string]int{"/a.md": 1}
	for i := 0; len(want) < 4; i++ {
		p := fmt.Sprintf("/d/%d.md", i)
		if graphstore.SourcePrefix(p, 1) != "0" {
			continue
		}
		var body strings.Builder
		fmt.Fprintf(&body, "# D%d\n\n", i)
		for j := range 1000 {
			fmt.Fprintf(&body, "[l](t/%d-%d.md) ", i, j)
		}
		h.alpha.publish(p, body.String())
		want[p] = 1
	}
	m := h.await(versions(want))
	if m.PrefixLength != 2 {
		t.Fatalf("prefix length = %d, want 2", m.PrefixLength)
	}
	old, err := h.hub.fetch(context.Background(), first.Shards[0].Path)
	if err != nil || !strings.Contains(old.Body, "> Sources: 0\n") {
		t.Errorf("old shard %s = %q, %v; want it rewritten empty", first.Shards[0].Path, old.Body, err)
	}
}
