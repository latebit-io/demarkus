package graphstore

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/protocol"
)

var updateWorldGolden = flag.Bool("update-world", false, "rewrite the world checkpoint contract fixture")

const worldGoldenDir = "testdata/world-graph"

// worldFixture is a world whose sources fall under four prefixes of
// length 1, one of them holding two sources.
func worldFixture() []WorldSource {
	return []WorldSource{
		{Path: "/index.md", Version: 4, Etag: "e-index-4", Title: "Hub", Links: 2, Edges: []WorldEdge{
			{To: "mark://latebit/docs/b.md", Count: 1},
			{To: "mark://latebit/docs/a.md", Label: "Applications", Count: 2},
		}},
		{Path: "/docs/a.md", Version: 2, Title: "Applications", Links: 1, Edges: []WorldEdge{
			{To: "mark://root/index.md", Anchor: "worlds", Count: 1},
		}},
		{Path: "/docs/b.md", Version: 3, Title: "B | pipes, \"quotes\" and ```fences```", Edges: []WorldEdge{
			{To: "mark://latebit/docs/a.md", Rel: "supersedes", Count: 1},
		}},
		{Path: "/adr/0001.md", Version: 1, Title: "First decision"},
		{Path: "/notes/c.md", Version: 7, Title: "Notes", Links: 1, Edges: []WorldEdge{
			{To: "mark://latebit/adr/0001.md", Count: 1},
		}},
	}
}

var worldFixtureCursor = protocol.Cursor{Epoch: "5f0c1e2a9b3d", Seq: 42}

var worldFixtureDerived = time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC)

// buildWorldCheckpoint builds world's shards at prefix length 1, pins each
// at version 1, and renders the manifest.
func buildWorldCheckpoint(t *testing.T, world string, sources []WorldSource) (manifest string, shards []WorldShard) {
	t.Helper()
	byPrefix := map[string][]WorldSource{}
	for _, src := range sources {
		prefix := SourcePrefix(src.Path, 1)
		byPrefix[prefix] = append(byPrefix[prefix], src)
	}
	m := WorldManifest{World: world, Cursor: worldFixtureCursor, Derived: worldFixtureDerived, Complete: true, PrefixLength: 1}
	for prefix, group := range byPrefix {
		shard, err := BuildWorldShard(world, prefix, group)
		if err != nil {
			t.Fatalf("build shard %s: %v", prefix, err)
		}
		shards = append(shards, shard)
		m.Shards = append(m.Shards, shard.Ref(1))
		m.Sources += shard.Sources
		m.Edges += shard.Edges
	}
	manifest, err := BuildWorldManifest(m)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	return manifest, shards
}

func published(body string, version int) protocol.Response {
	return protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: map[string]string{
		"version": strconv.Itoa(version), "content-hash": generation.BodyHash(body),
	}}
}

// TestWorldCheckpointContract pins the bytes the deriver publishes and the
// gateway seeds from: an unchanged source must render the same shard after
// an upgrade, or every checkpoint rewrites it. -update-world regenerates.
func TestWorldCheckpointContract(t *testing.T) {
	manifest, shards := buildWorldCheckpoint(t, "latebit", worldFixture())
	files := map[string]string{"manifest.md": manifest}
	for _, shard := range shards {
		files[shard.Prefix+".md"] = shard.Body
	}
	if *updateWorldGolden {
		if err := os.MkdirAll(worldGoldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(worldGoldenDir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(worldGoldenDir)
	if err != nil {
		t.Fatalf("read fixture (regenerate with -update-world): %v", err)
	}
	if len(entries) != len(files) {
		t.Fatalf("fixture has %d files, the producer renders %d", len(entries), len(files))
	}
	golden := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(worldGoldenDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		golden[entry.Name()] = string(data)
		if files[entry.Name()] != string(data) {
			t.Errorf("%s drifted from the contract fixture; if intentional, bump the format and regenerate with -update-world.\ngot:\n%s\nwant:\n%s", entry.Name(), files[entry.Name()], data)
		}
	}

	// The consumer half reads the fixture, not the producer's output.
	m, sources, err := LoadWorld("latebit", published(golden["manifest.md"], 9), func(path string) (protocol.Response, error) {
		base, version, ok := strings.Cut(path, "/v")
		if !ok || version != "1" {
			t.Fatalf("shard fetched as %q, want its pinned version 1", path)
		}
		return published(golden[strings.TrimPrefix(base, WorldGraphRoot+"/latebit/")], 1), nil
	})
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	if m.Cursor != worldFixtureCursor || !m.Derived.Equal(worldFixtureDerived) || !m.Complete || m.Sources != 5 || m.Edges != 5 || len(m.Shards) != 4 {
		t.Errorf("manifest = %+v", m)
	}
	want := map[string]WorldSource{}
	for _, src := range worldFixture() {
		for i := range src.Edges {
			src.Edges[i].Count = max(src.Edges[i].Count, 1)
		}
		want[src.Path] = src
	}
	for _, got := range sources {
		w := want[got.Path]
		if got.Version != w.Version || got.Etag != w.Etag || got.Title != w.Title || got.Links != w.Links || len(got.Edges) != len(w.Edges) {
			t.Errorf("source %s = %+v, want %+v", got.Path, got, w)
		}
		delete(want, got.Path)
	}
	if len(want) != 0 {
		t.Errorf("sources lost on the way: %v", want)
	}
}

func TestBuildWorldShardIgnoresInputOrder(t *testing.T) {
	sources := worldFixture()[3:] // the two sources under prefix 0
	forward, err := BuildWorldShard("latebit", "0", sources)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []WorldSource{sources[1], sources[0]}
	backward, err := BuildWorldShard("latebit", "0", reversed)
	if err != nil {
		t.Fatal(err)
	}
	if forward.Body != backward.Body {
		t.Fatalf("shard bytes depend on input order:\n%s\n%s", forward.Body, backward.Body)
	}
	hub := worldFixture()[0]
	hub.Edges[0], hub.Edges[1] = hub.Edges[1], hub.Edges[0]
	a, errA := BuildWorldShard("latebit", "f", worldFixture()[:1])
	b, errB := BuildWorldShard("latebit", "f", []WorldSource{hub})
	if errA != nil || errB != nil || a.Body != b.Body {
		t.Fatalf("shard bytes depend on edge order: %v %v", errA, errB)
	}
}

func TestBuildWorldShardRefuses(t *testing.T) {
	adr := worldFixture()[3] // prefix 0
	tests := []struct {
		name    string
		world   string
		prefix  string
		sources []WorldSource
	}{
		{name: "source under another prefix", world: "latebit", prefix: "1", sources: []WorldSource{adr}},
		{name: "repeated source", world: "latebit", prefix: "0", sources: []WorldSource{adr, adr}},
		{name: "path not canonical", world: "latebit", prefix: SourcePrefix("/adr/../x.md", 1), sources: []WorldSource{{Path: "/adr/../x.md", Version: 1}}},
		{name: "no version", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path}}},
		{name: "edge off mark", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path, Version: 1, Edges: []WorldEdge{{To: "https://example.com/x"}}}}},
		{name: "repeated edge", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path, Version: 1, Edges: []WorldEdge{{To: "mark://root/a.md"}, {To: "mark://root/a.md"}}}}},
		{name: "world not a DNS label", world: "Late_bit", prefix: "0", sources: []WorldSource{adr}},
		{name: "prefix not lowercase hex", world: "latebit", prefix: "A", sources: []WorldSource{adr}},
		{name: "prefix too long", world: "latebit", prefix: "08c0", sources: []WorldSource{adr}},
		{name: "over the body limit", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path, Version: 1, Title: strings.Repeat("x", protocol.MaxBodyLength)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuildWorldShard(tt.world, tt.prefix, tt.sources); err == nil {
				t.Fatal("built")
			}
		})
	}
}

func TestVerifyWorldShardRefuses(t *testing.T) {
	shard, err := BuildWorldShard("latebit", "0", worldFixture()[3:])
	if err != nil {
		t.Fatal(err)
	}
	ref := shard.Ref(3)
	if _, err := VerifyWorldShard("latebit", ref, published(shard.Body, 3)); err != nil {
		t.Fatalf("the shard as built: %v", err)
	}
	// rewritten keeps the pin honest for a body edited after the build, so
	// only the edit itself can fail verification.
	rewritten := func(body string) (WorldShardRef, protocol.Response) {
		edited := ref
		edited.ContentHash, edited.Bytes = generation.BodyHash(body), len(body)
		return edited, published(body, 3)
	}
	tests := []struct {
		name  string
		world string
		pin   func() (WorldShardRef, protocol.Response)
	}{
		{name: "another version", world: "latebit", pin: func() (WorldShardRef, protocol.Response) { return ref, published(shard.Body, 4) }},
		{name: "content differs from the pin", world: "latebit", pin: func() (WorldShardRef, protocol.Response) {
			return ref, published(strings.Replace(shard.Body, "Notes", "Nodes", 1), 3)
		}},
		{name: "another world", world: "root", pin: func() (WorldShardRef, protocol.Response) { return ref, published(shard.Body, 3) }},
		{name: "unknown field", world: "latebit", pin: func() (WorldShardRef, protocol.Response) {
			return rewritten(strings.Replace(shard.Body, `"links":`, `"extra":1,"links":`, 1))
		}},
		{name: "sources out of order", world: "latebit", pin: func() (WorldShardRef, protocol.Response) {
			swapped := strings.NewReplacer("/adr/0001.md", "/notes/c.md", "/notes/c.md", "/adr/0001.md").Replace(shard.Body)
			return rewritten(swapped)
		}},
		{name: "declared counts disagree", world: "latebit", pin: func() (WorldShardRef, protocol.Response) {
			return rewritten(strings.Replace(shard.Body, "> Sources: 2", "> Sources: 3", 1))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pin, resp := tt.pin()
			if _, err := VerifyWorldShard(tt.world, pin, resp); err == nil {
				t.Fatal("verified")
			}
		})
	}
}

func TestParseWorldManifestRefuses(t *testing.T) {
	manifest, _ := buildWorldCheckpoint(t, "latebit", worldFixture())
	if _, err := ParseWorldManifest("latebit", manifest); err != nil {
		t.Fatalf("the manifest as built: %v", err)
	}
	tests := []struct {
		name  string
		world string
		body  string
	}{
		{name: "another world", world: "root", body: manifest},
		{name: "another format", world: "latebit", body: strings.Replace(manifest, WorldManifestFormat, "demarkus-graph-world/v0", 1)},
		{name: "totals disagree", world: "latebit", body: strings.Replace(manifest, "> Sources: 5", "> Sources: 6", 1)},
		{name: "prefix length disagrees", world: "latebit", body: strings.Replace(manifest, "> Prefix-Length: 1", "> Prefix-Length: 2", 1)},
		{name: "bad cursor", world: "latebit", body: strings.Replace(manifest, "> Cursor: 5f0c1e2a9b3d:42", "> Cursor: 5f0c1e2a9b3d:042", 1)},
		{name: "shard path elsewhere", world: "latebit", body: strings.Replace(manifest, "/graph/worlds/latebit/0.md", "/graph/worlds/root/0.md", 1)},
		{name: "unknown metadata", world: "latebit", body: strings.Replace(manifest, "> Complete: true\n", "> Complete: true\n> Extra: 1\n", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseWorldManifest(tt.world, tt.body); err == nil {
				t.Fatal("parsed")
			}
		})
	}
}

func TestSourcePrefixIsTheSHA256OfThePath(t *testing.T) {
	// Pinned from `printf /index.md | shasum -a 256`: shard membership is wire format.
	if got := SourcePrefix("/index.md", 3); got != "f85" {
		t.Fatalf("SourcePrefix = %s, want f85", got)
	}
	if got := SourcePrefix("/adr/0001.md", 1); got != "0" {
		t.Fatalf("SourcePrefix = %s, want 0", got)
	}
}

func TestWorldRoundTripKeepsEveryField(t *testing.T) {
	src := worldFixture()[0]
	shard, err := BuildWorldShard("latebit", "f", []WorldSource{src})
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyWorldShard("latebit", shard.Ref(1), published(shard.Body, 1))
	if err != nil {
		t.Fatal(err)
	}
	src.Edges[0], src.Edges[1] = src.Edges[1], src.Edges[0] // edges come back sorted by target
	if !reflect.DeepEqual(got, []WorldSource{src}) {
		t.Fatalf("round trip = %+v, want %+v", got, src)
	}
}
