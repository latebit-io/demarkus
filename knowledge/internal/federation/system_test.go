package federation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/gateway"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/knowledgeserver/knowledgetest"
)

// Tests against real knowledge servers: each write to a world waits out the
// bucket store's commit interval, so they run in parallel.

const hubWorld, teamWorld = "hub", "team"

// system is one deployment: knowledge server replicas over shared buckets,
// serving the hub and the team world, each reached as a broker pod does.
type system struct {
	t       *testing.T
	buckets *knowledgetest.Buckets
	config  *core.Config
}

func newSystem(t *testing.T) *system {
	config := &core.Config{Worlds: []core.WorldConfig{
		{Name: hubWorld, Namespace: hubWorld, Profile: core.ProfileKnowledge, Local: true},
		{Name: teamWorld, Namespace: teamWorld, Profile: core.ProfileKnowledge, Local: true},
	}}
	return &system{t: t, buckets: &knowledgetest.Buckets{}, config: config}
}

// replica opens one more knowledge server and returns its in-process
// dispatcher.
func (s *system) replica() *gateway.Composite {
	all := s.config.Registry().All()
	worlds := make([]knowledgetest.World, len(all))
	for i := range all {
		worlds[i] = knowledgetest.World{Name: all[i].Name, Authority: all[i].Authority()}
	}
	server := knowledgetest.Open(s.t, s.buckets, worlds...)
	if err := gateway.CheckLocal(s.config.Registry(), server); err != nil {
		s.t.Fatal(err)
	}
	return gateway.NewComposite(s.config.Registry(), server, nil)
}

// deriver derives team from source into the hub through replica, as the
// broker wires it, logging to the test.
func (s *system) deriver(replica *gateway.Composite, source Source) *deriver {
	log := slog.New(slog.NewTextHandler(testLog{s.t}, nil))
	return testDeriver(Config{Worlds: []string{teamWorld}, Source: source, Hub: HubIO(replica, hubWorld), Log: log})
}

// lead runs a deriver until the returned stop, which waits for it to end.
func (s *system) lead(replica *gateway.Composite, source Source) (stop func()) {
	return runDeriver(s.t, s.deriver(replica, source))
}

// await polls until team's checkpoint, read through replica, satisfies check.
func (s *system) await(replica *gateway.Composite, check func(graphstore.WorldManifest, map[string]graphstore.WorldSource) error) graphstore.WorldManifest {
	s.t.Helper()
	return awaitCheckpoint(s.t, HubIO(replica, hubWorld), teamWorld, check)
}

// testLog writes a deriver's log to its test, shown when the test fails.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// publish writes body as docPath's next version in team.
func publish(t *testing.T, replica *gateway.Composite, docPath, body string) {
	t.Helper()
	ctx := protocol.WithGrant(context.Background(), protocol.Grant{Label: "test", Paths: []string{"/**"}})
	res, err := replica.Publish(ctx, fetch.WriteRequest{Host: teamWorld, Path: docPath, Body: body, ExpectedVersion: -1})
	if err != nil || !protocol.IsWriteSuccess(res.Response.Status) {
		t.Fatalf("publish %s: %q %v", docPath, res.Response.Status, err)
	}
}

// headCursor is team's feed position on replica.
func headCursor(t *testing.T, replica *gateway.Composite) protocol.Cursor {
	t.Helper()
	watch, err := replica.Watch(context.Background(), fetch.WatchRequest{Host: teamWorld, Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	return watch.Cursor()
}

// countingSource records the LISTs and FETCHes a deriver sends.
type countingSource struct {
	Source
	mu      sync.Mutex
	lists   int
	fetched []string
}

func (c *countingSource) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.Source.List(ctx, r)
}

func (c *countingSource) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	c.mu.Lock()
	c.fetched = append(c.fetched, r.Path)
	c.mu.Unlock()
	return c.Source.Fetch(ctx, r)
}

// A new leader on another replica resumes from the old leader's checkpoint:
// it lists nothing and reads only what changed since.
func TestNewLeaderResumesFromTheCheckpointOnAnotherReplica(t *testing.T) {
	t.Parallel()
	sys := newSystem(t)
	first, second := sys.replica(), sys.replica()
	publish(t, first, "/a.md", "# A\n\n[b](b.md)\n")
	publish(t, first, "/b.md", "# B\n")
	stop := sys.lead(first, first)
	sys.await(first, versions(map[string]int{"/a.md": 1, "/b.md": 1}))
	stop()

	counted := &countingSource{Source: second}
	sys.lead(second, counted)
	publish(t, second, "/c.md", "# C\n")
	sys.await(second, versions(map[string]int{"/a.md": 1, "/b.md": 1, "/c.md": 1}))
	counted.mu.Lock()
	defer counted.mu.Unlock()
	if counted.lists != 0 || !slices.Equal(counted.fetched, []string{"/c.md"}) {
		t.Errorf("new leader listed %d times and read %v, want no list and /c.md only", counted.lists, counted.fetched)
	}
}

// A deposed leader still writing cannot unpin the new leader's checkpoint:
// its shard lands as an unpinned version and its manifest loses the CAS.
func TestDeposedLeadersLateCheckpointIsHarmless(t *testing.T) {
	t.Parallel()
	ctx, sys := context.Background(), newSystem(t)
	first, second := sys.replica(), sys.replica()
	a, b := "/a.md", sameShard("/a.md")
	publish(t, first, a, "# A\n")
	publish(t, first, b, "# B\n")

	old := newWorld(sys.deriver(first, first), teamWorld)
	if since, err := old.load(ctx); err != nil || !since.IsZero() {
		t.Fatalf("first load: %v %v, want a rebuild", since, err)
	}
	if err := old.rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if err := old.checkpoint(ctx, headCursor(t, first)); err != nil {
		t.Fatal(err)
	}

	newer := newWorld(sys.deriver(second, second), teamWorld)
	if since, err := newer.load(ctx); err != nil || since.IsZero() {
		t.Fatalf("takeover load: %v %v, want a cursor", since, err)
	}
	publish(t, second, a, "# A2\n")
	applyAndCheckpoint(t, newer, second, a, 2)
	pinned := sys.await(second, versions(map[string]int{a: 2, b: 1}))

	publish(t, first, b, "# B2\n")
	applyEvent(t, old, b, 2, protocol.OpPublish)
	if err := old.checkpoint(ctx, headCursor(t, first)); !errors.Is(err, generation.ErrConflict) {
		t.Fatalf("the deposed leader's manifest write: %v, want a conflict", err)
	}
	m := sys.await(second, versions(map[string]int{a: 2, b: 1}))
	if !slices.Equal(m.Shards, pinned.Shards) || m.Cursor != pinned.Cursor {
		t.Fatalf("manifest moved to %+v, want %+v", m, pinned)
	}
	shard := pinned.Shards[0]
	head, err := HubIO(second, hubWorld).Fetch(ctx, shard.Path)
	if err != nil {
		t.Fatal(err)
	}
	if version, err := generation.ResponseVersion(shard.Path, head); err != nil || version <= shard.Version {
		t.Fatalf("shard head at version %d (%v), want past the pinned %d", version, err, shard.Version)
	}

	applyAndCheckpoint(t, newer, second, b, 2)
	sys.await(second, versions(map[string]int{a: 2, b: 2}))
}

// applyAndCheckpoint folds docPath's publish at version into w and
// checkpoints it at replica's head.
func applyAndCheckpoint(t *testing.T, w *world, replica *gateway.Composite, docPath string, version int) {
	t.Helper()
	applyEvent(t, w, docPath, version, protocol.OpPublish)
	if err := w.checkpoint(context.Background(), headCursor(t, replica)); err != nil {
		t.Fatal(err)
	}
}

// sameShard is a path other than docPath in docPath's shard at prefix
// length 1.
func sameShard(docPath string) string {
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("/doc-%d.md", i)
		if candidate != docPath && graphstore.SourcePrefix(candidate, 1) == graphstore.SourcePrefix(docPath, 1) {
			return candidate
		}
	}
}

// The hub takes the deriver's checkpoints, pruned to their retention, and
// nothing else under its grant.
func TestHubRefusesWritesOutsideTheFederationGrant(t *testing.T) {
	t.Parallel()
	ctx, hub := context.Background(), HubIO(newSystem(t).replica(), hubWorld)
	manifest := graphstore.WorldManifestPath(teamWorld)
	for _, tc := range []struct{ path, want string }{
		{manifest, protocol.StatusCreated},
		{graphstore.LegacyExportPath, protocol.StatusNotPermitted},
		{"/index.md", protocol.StatusNotPermitted},
	} {
		resp, err := hub.Publish(ctx, tc.path, "# Graph\n", -1)
		if err != nil || resp.Status != tc.want {
			t.Errorf("publish %s: %q %v, want %q", tc.path, resp.Status, err, tc.want)
		}
	}
	head, err := hub.Fetch(ctx, manifest)
	if err != nil || head.Metadata["retention"] != strconv.Itoa(retention) {
		t.Errorf("checkpoint metadata %v (%v), want retention %d", head.Metadata, err, retention)
	}
}
