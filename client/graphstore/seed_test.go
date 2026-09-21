package graphstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// seedRemote answers a seed pass from fixed documents and records each read.
type seedRemote struct {
	docs  map[string]protocol.Response
	err   error
	reads []string // "path etag"
}

func (r *seedRemote) fetch(_ context.Context, path, ifNoneMatch string) (protocol.Response, error) {
	r.reads = append(r.reads, path+" "+ifNoneMatch)
	if r.err != nil {
		return protocol.Response{}, r.err
	}
	if doc, ok := r.docs[path]; ok {
		return doc, nil
	}
	return protocol.Response{Status: protocol.StatusNotFound}, nil
}

func legacyExport(t *testing.T, from, to string) string {
	t.Helper()
	published := New()
	published.ReplaceSeed("publisher", []StoredNode{{URL: from, Status: "ok"}}, []StoredEdge{{From: from, To: to, Count: 1}})
	return published.Export()
}

func publishedSnapshot(t *testing.T, from, to string) func(context.Context, string, string) (protocol.Response, error) {
	t.Helper()
	remote := newSnapshotRemote()
	if _, err := PublishSnapshot(t.Context(), SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(),
		Nodes: []StoredNode{{URL: from, Status: "ok"}},
		Edges: []StoredEdge{{From: from, To: to, Count: 1}},
	}, remote.fetch, remote.publish); err != nil {
		t.Fatal(err)
	}
	remote.docs[SnapshotManifestPath].Metadata["etag"] = "snap-1"
	// Shards are read at their version paths, which only the remote's history holds.
	return func(ctx context.Context, path, _ string) (protocol.Response, error) { return remote.fetch(ctx, path) }
}

func TestSeedPrefersTheSnapshotAndFallsBackToTheLegacyExport(t *testing.T) {
	const from, to = "mark://w/a.md", "mark://w/b.md"
	legacy := protocol.Response{Status: protocol.StatusOK, Body: legacyExport(t, from, to), Metadata: map[string]string{"etag": "legacy-1"}}
	noSnapshot := &seedRemote{docs: map[string]protocol.Response{LegacyExportPath: legacy}}
	tests := []struct {
		name     string
		fetch    func(context.Context, string, string) (protocol.Response, error)
		wantEtag string
	}{
		{"snapshot", publishedSnapshot(t, from, to), "snap-1"},
		{"legacy export when no snapshot exists", noSnapshot.fetch, "legacy-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := New()
			store.Seed(t.Context(), SeedSource{Owner: "w", Fetch: tt.fetch})
			if got := store.Backlinks(to); len(got) != 1 || got[0] != from {
				t.Fatalf("backlinks = %v, want the seeded edge", got)
			}
			if store.SeedEtag("w") != tt.wantEtag {
				t.Errorf("etag = %q, want %q", store.SeedEtag("w"), tt.wantEtag)
			}
		})
	}
	if len(noSnapshot.reads) != 2 {
		t.Errorf("reads = %v, want the manifest, then the legacy export", noSnapshot.reads)
	}
}

func TestSeedSendsTheEtagAndKeepsACurrentSeed(t *testing.T) {
	store := New()
	store.ReplaceSeed("w", []StoredNode{{URL: "mark://w/a.md", Status: "ok"}}, []StoredEdge{{From: "mark://w/a.md", To: "mark://w/b.md", Count: 1}})
	store.SetSeedEtag("w", "snap-1")
	remote := &seedRemote{docs: map[string]protocol.Response{SnapshotManifestPath: {Status: protocol.StatusNotModified}}}
	store.Seed(t.Context(), SeedSource{Owner: "w", Fetch: remote.fetch})
	if len(remote.reads) != 1 || remote.reads[0] != SnapshotManifestPath+" snap-1" {
		t.Errorf("reads = %v, want one conditional manifest read", remote.reads)
	}
	if len(store.Backlinks("mark://w/b.md")) != 1 {
		t.Error("a current seed was dropped")
	}
}

// Absent is a fine answer; unreadable is not: the old seed stays, marked.
func TestSeedFailureKeepsTheLastGoodSeedAndReportsWhy(t *testing.T) {
	tests := []struct {
		name   string
		remote *seedRemote
		failed bool
	}{
		{"nothing published", &seedRemote{}, false},
		{"transport", &seedRemote{err: errors.New("dial refused")}, true},
		{"snapshot status", &seedRemote{docs: map[string]protocol.Response{SnapshotManifestPath: {Status: protocol.StatusServerError}}}, true},
		{"snapshot unreadable", &seedRemote{docs: map[string]protocol.Response{SnapshotManifestPath: {Status: protocol.StatusOK, Body: "not a manifest"}}}, true},
		{"legacy status", &seedRemote{docs: map[string]protocol.Response{LegacyExportPath: {Status: protocol.StatusServerError}}}, true},
		{"legacy unreadable", &seedRemote{docs: map[string]protocol.Response{LegacyExportPath: {Status: protocol.StatusOK, Body: "## Nodes\n- broken"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := New()
			store.ReplaceSeed("w", []StoredNode{{URL: "mark://w/a.md", Status: "ok"}}, []StoredEdge{{From: "mark://w/a.md", To: "mark://w/b.md", Count: 1}})
			var problems []SeedProblem
			store.Seed(t.Context(), SeedSource{Owner: "w", Fetch: tt.remote.fetch, Problem: func(p SeedProblem) { problems = append(problems, p) }})
			if len(store.Backlinks("mark://w/b.md")) != 1 {
				t.Error("the last good seed was dropped")
			}
			node := store.GetNode("mark://w/a.md")
			if marked := node.Observation.Problem == "seed-refresh-failed"; marked != tt.failed || (len(problems) == 1) != tt.failed {
				t.Errorf("marked = %v, problems = %v, want failed = %v", marked, problems, tt.failed)
			}
		})
	}
}

// A surface may translate or drop rows before they become the owner's seed.
func TestSeedRewriteRunsBeforeTheSeedIsReplaced(t *testing.T) {
	legacy := protocol.Response{Status: protocol.StatusOK, Body: legacyExport(t, "mark://w.svc/a.md", "mark://w.svc/b.md")}
	store, remote := New(), &seedRemote{docs: map[string]protocol.Response{LegacyExportPath: legacy}}
	store.Seed(t.Context(), SeedSource{Owner: "w", Fetch: remote.fetch,
		Rewrite: func([]StoredNode, []StoredEdge) ([]StoredNode, []StoredEdge) {
			return nil, nil // a filter that owns nothing
		}})
	if n := store.NodeCount(); n != 0 {
		t.Errorf("nodes = %d, want the filtered rows kept out", n)
	}
}

// A store that cannot be written loses the failure mark on restart. That is a
// second problem, reported beside the first, never instead of it or not at all.
func TestSeedReportsASaveFailureBesideTheSeedProblem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read only directory")
	}
	dir := t.TempDir()
	store, err := Load(filepath.Join(dir, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Loadable, then not writable: Save cannot create its temporary file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore %s: %v", dir, err)
		}
	})
	remote := &seedRemote{err: errors.New("dial refused")}
	var steps []string
	store.Seed(t.Context(), SeedSource{Owner: "w", Fetch: remote.fetch, Problem: func(p SeedProblem) { steps = append(steps, p.Step) }})
	if !slices.Equal(steps, []string{"fetch", "save"}) {
		t.Errorf("problems = %v, want the seed problem and the save problem", steps)
	}
}
