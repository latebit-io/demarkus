package graphstore

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/protocol"
)

var updateWorldGolden = flag.Bool("update-world", false, "rewrite the world checkpoint contract fixture")

const worldGoldenDir = "testdata/world-graph"

// worldFixture is a world whose sources fall under four prefixes of length
// 1, one holding two sources, written in the order a shard stores them.
func worldFixture() []WorldSource {
	return []WorldSource{
		{Path: "/index.md", Version: 4, Etag: "e-index-4", Title: "Hub", Edges: []WorldEdge{
			{To: "mark://latebit/docs/a.md", Label: "Applications", Count: 2},
			{To: "mark://latebit/docs/b.md", Count: 1},
		}},
		{Path: "/docs/a.md", Version: 2, Title: "Applications", Edges: []WorldEdge{
			{To: "mark://root/index.md", Anchor: "worlds", Count: 1},
		}},
		{Path: "/docs/b.md", Version: 3, Title: "B | pipes, \"quotes\" and ```fences```", Edges: []WorldEdge{
			{To: "mark://latebit/docs/a.md", Rel: "supersedes", Count: 1},
		}},
		{Path: "/adr/0001.md", Version: 1, Title: "First decision"},
		{Path: "/notes/c.md", Version: 7, Title: "Notes", Edges: []WorldEdge{
			{To: "mark://latebit/adr/0001.md", Count: 1},
		}},
	}
}

var worldFixtureCursor = protocol.Cursor{Epoch: "5f0c1e2a9b3d", Seq: 42}

// buildWorldCheckpoint builds world's shards at prefix length 1, pins each
// at version 1, and renders the manifest.
func buildWorldCheckpoint(t *testing.T, world string, sources []WorldSource) (manifest string, shards []WorldShard) {
	t.Helper()
	byPrefix := map[string][]WorldSource{}
	for _, src := range sources {
		prefix := SourcePrefix(src.Path, 1)
		byPrefix[prefix] = append(byPrefix[prefix], src)
	}
	m := WorldManifest{World: world, Cursor: worldFixtureCursor, Complete: true, PrefixLength: 1}
	for prefix, group := range byPrefix {
		shard, err := BuildWorldShard(world, prefix, group)
		if err != nil {
			t.Fatalf("build shard %s: %v", prefix, err)
		}
		shards = append(shards, shard)
		m.Shards = append(m.Shards, shard.Ref(1))
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
	manifestFile := path.Base(WorldManifestPath("latebit"))
	manifest, shards := buildWorldCheckpoint(t, "latebit", worldFixture())
	files := map[string]string{manifestFile: manifest}
	for _, shard := range shards {
		files[path.Base(shard.Path)] = shard.Body
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
	load, err := LoadWorld(context.Background(), WorldLoadRequest{World: "latebit", Manifest: published(golden[manifestFile], 9), Fetch: func(_ context.Context, versioned string) (protocol.Response, error) {
		base, version, ok := strings.Cut(versioned, "/v")
		if !ok || version != "1" {
			return protocol.Response{}, fmt.Errorf("shard fetched as %q, want its pinned version 1", versioned)
		}
		return published(golden[path.Base(base)], 1), nil
	}})
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	m, sources := load.Manifest, load.Sources
	if m.Cursor != worldFixtureCursor || !m.Complete || m.Sources != 5 || m.Edges != 5 || len(m.Shards) != 4 {
		t.Errorf("manifest = %+v", m)
	}
	want := map[string]WorldSource{}
	for _, src := range worldFixture() {
		want[src.Path] = src
	}
	for _, got := range sources {
		if !reflect.DeepEqual(got, want[got.Path]) {
			t.Errorf("source %s = %+v, want %+v", got.Path, got, want[got.Path])
		}
		delete(want, got.Path)
	}
	if len(want) != 0 {
		t.Errorf("sources lost on the way: %v", want)
	}
}

func TestBuildWorldShardIgnoresInputOrder(t *testing.T) {
	sources := worldFixture()[3:] // the two sources under prefix 0
	forward, errF := BuildWorldShard("latebit", "0", sources)
	backward, errB := BuildWorldShard("latebit", "0", []WorldSource{sources[1], sources[0]})
	if errF != nil || errB != nil || forward.Body != backward.Body {
		t.Fatalf("shard bytes depend on source order: %v %v", errF, errB)
	}
	hub := worldFixture()[0]
	hub.Edges[0], hub.Edges[1] = hub.Edges[1], hub.Edges[0]
	a, errA := BuildWorldShard("latebit", "f", worldFixture()[:1])
	b, errB := BuildWorldShard("latebit", "f", []WorldSource{hub})
	if errA != nil || errB != nil || a.Body != b.Body {
		t.Fatalf("shard bytes depend on edge order: %v %v", errA, errB)
	}
}

// A hub that cannot serve a pin now is retried; a pin it no longer has, or
// a shard that does not verify, is an invalid checkpoint.
func TestLoadWorldTellsUnavailableFromInvalid(t *testing.T) {
	manifest, shards := buildWorldCheckpoint(t, "latebit", worldFixture())
	for _, tt := range []struct {
		name        string
		shard       protocol.Response
		err         error
		unavailable bool
	}{
		{name: "fetch failed", err: errors.New("hub down"), unavailable: true},
		{name: "rate limited", shard: protocol.Response{Status: protocol.StatusRateLimited}, unavailable: true},
		{name: "pruned", shard: protocol.Response{Status: protocol.StatusNotFound}},
		{name: "tampered", shard: published(shards[0].Body+"\n", 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadWorld(context.Background(), WorldLoadRequest{World: "latebit", Manifest: published(manifest, 1), Fetch: func(context.Context, string) (protocol.Response, error) { return tt.shard, tt.err }})
			if err == nil || errors.Is(err, ErrWorldUnavailable) != tt.unavailable {
				t.Errorf("err = %v, unavailable %t", err, tt.unavailable)
			}
		})
	}
	if _, err := LoadWorld(context.Background(), WorldLoadRequest{World: "latebit", Manifest: protocol.Response{Status: protocol.StatusServerError}}); !errors.Is(err, ErrWorldUnavailable) {
		t.Errorf("manifest server error = %v, want unavailable", err)
	}
}

// An emptied shard is rewritten, not archived, so it must still build.
func TestBuildWorldShardEmpty(t *testing.T) {
	empty, err := BuildWorldShard("latebit", "0", nil)
	if err != nil || empty.Sources != 0 || !strings.Contains(empty.Body, `{"sources":[]}`) {
		t.Fatalf("empty shard = %+v, %v", empty, err)
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
		{name: "title over two lines", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path, Version: 1, Title: "a\nb"}}},
		{name: "edge off mark", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path, Version: 1, Edges: []WorldEdge{{To: "https://example.com/x"}}}}},
		{name: "repeated edge", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path, Version: 1, Edges: []WorldEdge{{To: "mark://root/a.md"}, {To: "mark://ROOT:6309/a.md"}}}}},
		{name: "world not a DNS label", world: "Late_bit", prefix: "0", sources: []WorldSource{adr}},
		{name: "prefix not lowercase hex", world: "latebit", prefix: "A", sources: []WorldSource{adr}},
		{name: "prefix too long", world: "latebit", prefix: "08c0", sources: []WorldSource{adr}},
		{name: "over the body limit", world: "latebit", prefix: "0", sources: []WorldSource{{Path: adr.Path, Version: 1, Title: strings.Repeat("x", protocol.MaxBodyLength)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildWorldShard(tt.world, tt.prefix, tt.sources)
			if err == nil {
				t.Fatal("built")
			}
			if oversized := tt.name == "over the body limit"; errors.Is(err, ErrWorldShardTooLarge) != oversized {
				t.Errorf("err = %v, ErrWorldShardTooLarge only when oversized", err)
			}
		})
	}
}

func TestVerifyWorldShardRefuses(t *testing.T) {
	shard, err := BuildWorldShard("latebit", "0", worldFixture()[3:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWorldShard("latebit", shard.Ref(3), published(shard.Body, 3)); err != nil {
		t.Fatalf("the shard as built: %v", err)
	}
	tests := []struct {
		name    string
		world   string
		prefix  string
		body    string
		version int
		repin   bool // pin the edited body, so only the edit can fail
	}{
		{name: "another version", version: 4},
		{name: "content differs from the pin", body: strings.Replace(shard.Body, "Notes", "Nodes", 1)},
		{name: "another world", world: "root"},
		{name: "prefix past the maximum", prefix: "08c0"},
		{name: "unknown field", body: strings.Replace(shard.Body, `"version":1,`, `"version":1,"extra":1,`, 1), repin: true},
		{name: "sources out of order", body: strings.NewReplacer("/adr/0001.md", "/notes/c.md", "/notes/c.md", "/adr/0001.md").Replace(shard.Body), repin: true},
		{name: "spacing the builder does not write", body: strings.Replace(shard.Body, `{"sources":`, `{ "sources":`, 1), repin: true},
		{name: "declared counts disagree", body: strings.Replace(shard.Body, "> Sources: 2", "> Sources: 3", 1), repin: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world, body, version, pin := cmp.Or(tt.world, "latebit"), cmp.Or(tt.body, shard.Body), cmp.Or(tt.version, 3), shard.Ref(3)
			pin.Prefix = cmp.Or(tt.prefix, pin.Prefix)
			if tt.repin {
				pin.ContentHash, pin.Bytes = generation.BodyHash(body), len(body)
			}
			if _, err := VerifyWorldShard(world, pin, published(body, version)); err == nil {
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
		{name: "content hash as a path", world: "latebit", body: strings.Replace(manifest, "| sha256-", "| /sha256-", 1)},
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

func TestGeneratedGraphPaths(t *testing.T) {
	for _, docPath := range []string{"/graph.md", "/graph.md/v3", "/graph/manifest.md", "/graph/manifest.md/v2", "/graph/shards/a/nodes-000.md", WorldManifestPath("latebit"), WorldShardPath("latebit", "0f")} {
		if !IsGeneratedGraphPath(docPath) {
			t.Errorf("generated path %q was not recognized", docPath)
		}
	}
	for _, docPath := range []string{"/graph/authored.md", "/graph/worlds.md", "/graphs/x.md"} {
		if IsGeneratedGraphPath(docPath) {
			t.Errorf("authored path %q was treated as generated", docPath)
		}
	}
}
