package index

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

const testHashA = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testHashB = "sha256-abbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestShardedManifestRoundTrip(t *testing.T) {
	artifacts, err := BuildShards("/index.md", SlotA, []Entry{
		{Hash: testHashB, Server: "mark://b", Path: "/b.md"},
		{Hash: testHashA, Server: "mark://a", Path: "/\ta|one.md\u00a0 "},
	}, 0)
	if err != nil {
		t.Fatalf("BuildShards: %v", err)
	}
	refs := make([]ShardRef, len(artifacts))
	for i, artifact := range artifacts {
		refs[i] = artifact.Ref(i + 3)
	}
	m := Manifest{
		Source: "aggregated", Indexed: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		Complete: true, Documents: 2, ActiveSlot: SlotA, Shards: refs,
	}
	body, err := BuildManifest("/index.md", m)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	got, err := ParseManifest("/index.md", body)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if got.Source != m.Source || got.Documents != 2 || got.ActiveSlot != SlotA || len(got.Shards) != 2 {
		t.Fatalf("manifest = %+v", got)
	}
	if !strings.Contains(body, "/index.shards/a/aa-000.md") || !strings.Contains(body, "/index.shards/a/ab-000.md") {
		t.Fatalf("manifest paths missing:\n%s", body)
	}
	_, _, parsedEntries, err := parseShard(artifacts[0].Body)
	if err != nil {
		t.Fatalf("parseShard: %v", err)
	}
	if parsedEntries[0].Path != "/\ta|one.md\u00a0 " {
		t.Fatalf("path = %q, want whitespace-preserving round trip", parsedEntries[0].Path)
	}
}

func TestBuildShardsDeterministicOverflow(t *testing.T) {
	entries := make([]Entry, 8)
	for i := range entries {
		entries[i] = Entry{Hash: testHashA, Server: "mark://server", Path: fmt.Sprintf("/doc-%02d.md", 7-i)}
	}
	one, err := BuildShards("/indexes/all.md", SlotB, entries, 330)
	if err != nil {
		t.Fatalf("BuildShards: %v", err)
	}
	two, err := BuildShards("/indexes/all.md", SlotB, entries, 330)
	if err != nil {
		t.Fatalf("BuildShards repeat: %v", err)
	}
	if len(one) < 2 || len(one) != len(two) {
		t.Fatalf("parts = %d and %d", len(one), len(two))
	}
	for i := range one {
		if one[i].Body != two[i].Body || one[i].Path != two[i].Path || one[i].Bytes > 330 {
			t.Fatalf("part %d differs or exceeds target: %+v", i, one[i])
		}
		if !strings.Contains(one[i].Path, fmt.Sprintf("/b/aa-%03d.md", i)) {
			t.Errorf("part path = %q", one[i].Path)
		}
	}
}

func TestEntriesForHashFetchesOnlyMatchingVerifiedShards(t *testing.T) {
	artifacts, err := BuildShards("/index.md", SlotA, []Entry{
		{Hash: testHashA, Server: "mark://a", Path: "/a.md"},
		{Hash: testHashB, Server: "mark://b", Path: "/b.md"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]ShardRef, len(artifacts))
	responses := make(map[string]protocol.Response)
	for i, artifact := range artifacts {
		refs[i] = artifact.Ref(i + 1)
		responses[VersionPath(artifact.Path, i+1)] = protocol.Response{Status: protocol.StatusOK, Body: artifact.Body, Metadata: map[string]string{
			"version": strconvI(i + 1), "content-hash": artifact.ContentHash,
		}}
	}
	manifestBody, err := BuildManifest("/index.md", Manifest{
		Source: "aggregated", Indexed: time.Now(), Complete: true,
		Documents: 2, ActiveSlot: SlotA, Shards: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fetched []string
	matches, err := EntriesForHash("/index.md", manifestBody, testHashA, func(path string) (protocol.Response, error) {
		fetched = append(fetched, path)
		return responses[path], nil
	})
	if err != nil {
		t.Fatalf("EntriesForHash: %v", err)
	}
	if len(matches) != 1 || matches[0].Path != "/a.md" || len(fetched) != 1 || !strings.Contains(fetched[0], "/aa-") {
		t.Fatalf("matches = %+v, fetched = %v", matches, fetched)
	}
}

func TestVerifyShardRejectsDrift(t *testing.T) {
	artifacts, err := BuildShards("/index.md", SlotA, []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := artifacts[0]
	ref := a.Ref(2)
	tests := []protocol.Response{
		{Status: protocol.StatusOK, Body: a.Body, Metadata: map[string]string{"version": "3", "content-hash": a.ContentHash}},
		{Status: protocol.StatusOK, Body: a.Body + "x", Metadata: map[string]string{"version": "2", "content-hash": a.ContentHash}},
		{Status: protocol.StatusOK, Body: a.Body, Metadata: map[string]string{"version": "2", "content-hash": testHashB}},
	}
	for i, resp := range tests {
		if _, err := VerifyShard(ref, resp); err == nil {
			t.Errorf("case %d accepted drift", i)
		}
	}
}

func TestVerifyShardRejectsPartPathMismatch(t *testing.T) {
	artifacts, err := BuildShards("/index.md", SlotA, []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(artifacts[0].Body, "> Part: 0", "> Part: 1", 1)
	ref := artifacts[0].Ref(1)
	ref.ContentHash = BodyHash(body)
	ref.Bytes = len(body)
	resp := protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: map[string]string{
		"version": "1", "content-hash": ref.ContentHash,
	}}
	if _, err := VerifyShard(ref, resp); err == nil {
		t.Fatal("VerifyShard accepted part/path mismatch")
	}
}

func TestBuildShardsExactTargetBoundary(t *testing.T) {
	entry := []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}}
	base, err := BuildShards("/index.md", SlotA, entry, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildShards("/index.md", SlotA, entry, base[0].Bytes); err != nil {
		t.Fatalf("exact boundary rejected: %v", err)
	}
	if _, err := BuildShards("/index.md", SlotA, entry, base[0].Bytes-1); err == nil {
		t.Fatal("oversized single row accepted")
	}
}

func TestEntriesForHashReadsLegacyIndex(t *testing.T) {
	body := Build("legacy", time.Now(), []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}})
	matches, err := EntriesForHash("/index.md", body, testHashA, func(string) (protocol.Response, error) {
		t.Fatal("legacy index fetched a shard")
		return protocol.Response{}, nil
	})
	if err != nil || len(matches) != 1 {
		t.Fatalf("matches = %+v, err = %v", matches, err)
	}
}

func TestBuildShardsRejectsUnsafeManifestPath(t *testing.T) {
	entry := []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}}
	for _, manifestPath := range []string{"index.md", "/a/../index.md", "/"} {
		if _, err := BuildShards(manifestPath, SlotA, entry, 0); err == nil {
			t.Errorf("BuildShards accepted %q", manifestPath)
		}
	}
}

func TestParseManifestRejectsMissingPart(t *testing.T) {
	indexed := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	refs := []ShardRef{
		{Prefix: "aa", Path: "/index.shards/a/aa-000.md", Version: 1, ContentHash: testHashA, Rows: 1, Bytes: 100},
		{Prefix: "aa", Path: "/index.shards/a/aa-002.md", Version: 1, ContentHash: testHashB, Rows: 1, Bytes: 100},
	}
	if _, err := BuildManifest("/index.md", Manifest{
		Source: "aggregated", Indexed: indexed, Complete: true, Documents: 2, ActiveSlot: SlotA, Shards: refs,
	}); err == nil {
		t.Fatal("BuildManifest accepted missing overflow part")
	}
}

func TestParseManifestRejectsAmbiguousMetadata(t *testing.T) {
	body := `# Content Index Manifest

> Format: demarkus-hash-index/v2
> Format: demarkus-hash-index/v2
`
	if _, err := ParseManifest("/index.md", body); err == nil {
		t.Fatal("ParseManifest accepted duplicate metadata")
	}
}

func TestParseManifestRejectsHugeCountsWithoutAllocation(t *testing.T) {
	body := fmt.Sprintf(`# Content Index Manifest

> Format: %s
> Source: aggregated
> Indexed: 2026-08-25T10:00:00Z
> Complete: true
> Documents: %d
> Active-Slot: a

| Prefix | Path | Version | Content Hash | Rows | Bytes |
|--------|------|---------|--------------|------|-------|
| aa | /index.shards/a/aa-000.md | 1 | %s | %d | 1 |
`, ManifestFormat, int(^uint(0)>>1), testHashA, int(^uint(0)>>1))
	if _, err := ParseManifest("/index.md", body); err == nil {
		t.Fatal("huge manifest unexpectedly parsed")
	}
}

func TestGeneratedParsersRejectPreambleAndBadSeparator(t *testing.T) {
	body := `# Content Index Manifest

unexpected prose
> Format: demarkus-hash-index/v2
`
	if _, err := ParseManifest("/index.md", body); err == nil {
		t.Fatal("ParseManifest accepted unexpected preamble")
	}
	body = strings.Replace(mustManifestBody(t), "|--------|------|---------|--------------|------|-------|", "|x|x|x|x|x|x|", 1)
	if _, err := ParseManifest("/index.md", body); err == nil {
		t.Fatal("ParseManifest accepted invalid separator")
	}
}

func TestBuildShardsRejectsInvalidLocations(t *testing.T) {
	for _, entry := range []Entry{
		{Hash: testHashA, Server: "mark://user@host", Path: "/a.md"},
		{Hash: testHashA, Server: "mark://host:70000", Path: "/a.md"},
		{Hash: testHashA, Server: "mark://host", Path: "/a.md\nnext"},
	} {
		if _, err := BuildShards("/index.md", SlotA, []Entry{entry}, 0); err == nil {
			t.Errorf("BuildShards accepted %+v", entry)
		}
	}
}

func mustManifestBody(t *testing.T) string {
	t.Helper()
	body, err := BuildManifest("/index.md", Manifest{
		Source: "aggregated", Indexed: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		Complete: true, Documents: 0, ActiveSlot: SlotA,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestEntriesForHashNormalizesLeadingSlash(t *testing.T) {
	body := Build("legacy", time.Now(), []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}})
	matches, err := EntriesForHash("/index.md", body, "/"+testHashA, bodyFetcherPanic(t))
	if err != nil || len(matches) != 1 {
		t.Fatalf("matches = %+v, err = %v", matches, err)
	}
}

func bodyFetcherPanic(t *testing.T) func(string) (protocol.Response, error) {
	t.Helper()
	return func(string) (protocol.Response, error) {
		t.Fatal("legacy index fetched a shard")
		return protocol.Response{}, nil
	}
}

func strconvI(value int) string {
	return fmt.Sprintf("%d", value)
}
