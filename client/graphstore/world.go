package graphstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// A world checkpoint is a manifest plus shards that partition the world's
// sources by a prefix of sha256(path), so one changed document rewrites one
// shard. Bodies are deterministic: readers verify by rebuilding them.
const (
	// WorldGraphRoot holds one directory of checkpoint documents per world.
	WorldGraphRoot = "/graph/worlds"
	// WorldManifestFormat identifies the world manifest wire format.
	WorldManifestFormat = "demarkus-graph-world/v1"
	// WorldShardFormat identifies the world shard wire format.
	WorldShardFormat = "demarkus-graph-world-shard/v1"
	// MaxWorldPrefixLength bounds a world at 16^3 shards.
	MaxWorldPrefixLength = 3
	worldManifestTitle   = "# Graph World Manifest"
	worldShardTitle      = "# Graph World Shard"
)

// ErrWorldShardTooLarge is a shard over the body limit: its world needs a
// longer prefix.
var ErrWorldShardTooLarge = errors.New("world shard over the body limit")

// ErrWorldUnavailable is a checkpoint the hub could not serve for now: a
// failed fetch, or a status other than ok or not-found. Any other load
// failure means the checkpoint is invalid.
var ErrWorldUnavailable = errors.New("world checkpoint unavailable")

var worldManifestHeader = []string{"Prefix", "Path", "Version", "Content Hash", "Sources", "Edges", "Bytes"}

// WorldSource is one document of a world as derived at Version, with the
// edges it links out with. Its node's LinkCount is the sum of Count over its
// edges without Rel.
type WorldSource struct {
	Path    string      `json:"path"`
	Version int         `json:"version"`
	Etag    string      `json:"etag,omitempty"`
	Title   string      `json:"title,omitempty"`
	Edges   []WorldEdge `json:"edges,omitempty"`
}

// WorldEdge is one outgoing edge of a source, which is its origin.
type WorldEdge struct {
	To     string `json:"to"`
	Rel    string `json:"rel,omitempty"`
	Label  string `json:"label,omitempty"`
	Anchor string `json:"anchor,omitempty"`
	Count  int    `json:"count"`
}

type worldShardJSON struct {
	Sources []WorldSource `json:"sources"`
}

// WorldManifest names one world's checkpoint: every non-empty shard pinned at
// its version, and the change-feed cursor the shards are complete through.
type WorldManifest struct {
	World        string
	Cursor       protocol.Cursor
	Complete     bool
	PrefixLength int
	Sources      int
	Edges        int
	Shards       []WorldShardRef
}

// WorldShardRef pins one published shard.
type WorldShardRef struct {
	Prefix      string
	Path        string
	Version     int
	ContentHash string
	Sources     int
	Edges       int
	Bytes       int
}

func (ref *WorldShardRef) pin() generation.Pin {
	return generation.Pin{Path: ref.Path, Version: ref.Version, ContentHash: ref.ContentHash, Bytes: ref.Bytes}
}

// WorldShard is a built shard: its ref before the version is known, and body.
type WorldShard struct {
	WorldShardRef
	Body string
}

// Ref pins the shard at its published version.
func (s *WorldShard) Ref(version int) WorldShardRef {
	ref := s.WorldShardRef
	ref.Version = version
	return ref
}

// WorldManifestPath is where world's manifest is published in the hub.
func WorldManifestPath(world string) string { return WorldGraphRoot + "/" + world + "/manifest.md" }

// WorldShardPath is where world's shard for prefix is published in the hub.
func WorldShardPath(world, prefix string) string {
	return WorldGraphRoot + "/" + world + "/" + prefix + ".md"
}

// SourcePrefix is the first length hex digits of sha256(path).
func SourcePrefix(path string, length int) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:2])[:length]
}

// BuildWorldShard renders the shard of world's sources under prefix, sorted
// and canonicalized so equal sources always render equal bytes.
func BuildWorldShard(world, prefix string, sources []WorldSource) (WorldShard, error) {
	if !protocol.IsWorldName(world) {
		return WorldShard{}, fmt.Errorf("invalid world name %q", world)
	}
	if err := validateWorldPrefix(prefix, len(prefix)); err != nil {
		return WorldShard{}, err
	}
	rows := append(make([]WorldSource, 0, len(sources)), sources...) // [] when empty, never null
	for i := range rows {
		edges := slices.Clone(rows[i].Edges)
		for j := range edges {
			target, err := links.ParseMark(edges[j].To)
			if err != nil {
				return WorldShard{}, fmt.Errorf("source %q: edge target %q: %w", rows[i].Path, edges[j].To, err)
			}
			edges[j].To, edges[j].Count = target.NodeURL(), max(edges[j].Count, 1)
		}
		slices.SortFunc(edges, compareWorldEdges)
		rows[i].Edges = edges
	}
	slices.SortFunc(rows, func(a, b WorldSource) int { return strings.Compare(a.Path, b.Path) })
	edges, err := validateWorldSources(prefix, rows)
	if err != nil {
		return WorldShard{}, err
	}
	encoded, err := json.Marshal(worldShardJSON{Sources: rows})
	if err != nil {
		return WorldShard{}, fmt.Errorf("encode world shard %s/%s: %w", world, prefix, err)
	}
	var b strings.Builder
	b.Grow(len(encoded) + 256)
	fmt.Fprintf(&b, "%s\n\n> Format: %s\n> World: %s\n> Prefix: %s\n> Sources: %d\n> Edges: %d\n",
		worldShardTitle, WorldShardFormat, world, prefix, len(rows), edges)
	b.WriteString(snapshotJSONFence)
	b.Write(encoded)
	b.WriteString(snapshotJSONFenceEnd)
	body := b.String()
	if len(body) > protocol.MaxBodyLength {
		return WorldShard{}, fmt.Errorf("world shard %s/%s is %d bytes: %w", world, prefix, len(body), ErrWorldShardTooLarge)
	}
	ref := WorldShardRef{Prefix: prefix, Path: WorldShardPath(world, prefix), ContentHash: generation.BodyHash(body), Sources: len(rows), Edges: edges, Bytes: len(body)}
	return WorldShard{WorldShardRef: ref, Body: body}, nil
}

// VerifyWorldShard checks a fetched shard against its pin and returns its
// sources. Only the bytes BuildWorldShard renders for them are accepted.
func VerifyWorldShard(world string, ref WorldShardRef, resp protocol.Response) ([]WorldSource, error) { //nolint:gocritic // a pin is passed once per shard
	if err := validateWorldPrefix(ref.Prefix, len(ref.Prefix)); err != nil {
		return nil, fmt.Errorf("world shard %s: %w", ref.Path, err)
	}
	if err := generation.VerifyPinned(ref.pin(), resp); err != nil {
		return nil, fmt.Errorf("world shard: %w", err)
	}
	_, payloadText, err := parseSnapshotDocument(resp.Body, worldShardTitle)
	if err != nil {
		return nil, fmt.Errorf("world shard %s: %w", ref.Path, err)
	}
	var payload worldShardJSON
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		return nil, fmt.Errorf("world shard %s payload: %w", ref.Path, err)
	}
	rebuilt, err := BuildWorldShard(world, ref.Prefix, payload.Sources)
	if err != nil {
		return nil, fmt.Errorf("world shard %s: %w", ref.Path, err)
	}
	if rebuilt.Body != resp.Body || rebuilt.Path != ref.Path || rebuilt.Sources != ref.Sources || rebuilt.Edges != ref.Edges {
		return nil, fmt.Errorf("world shard %s is not the shard its sources build", ref.Path)
	}
	return payload.Sources, nil
}

// BuildWorldManifest renders m with its shards in prefix order and its
// totals summed from them.
func BuildWorldManifest(m WorldManifest) (string, error) { //nolint:gocritic // local copy is sorted without mutating the caller
	m.Shards = slices.Clone(m.Shards)
	slices.SortFunc(m.Shards, func(a, b WorldShardRef) int { return strings.Compare(a.Prefix, b.Prefix) })
	m.Sources, m.Edges = 0, 0
	for i := range m.Shards {
		m.Sources += m.Shards[i].Sources
		m.Edges += m.Shards[i].Edges
	}
	if err := validateWorldManifest(&m); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n> Format: %s\n> World: %s\n> Cursor: %s\n> Complete: %t\n> Prefix-Length: %d\n> Sources: %d\n> Edges: %d\n\n",
		worldManifestTitle, WorldManifestFormat, m.World, m.Cursor, m.Complete, m.PrefixLength, m.Sources, m.Edges)
	b.WriteString("| " + strings.Join(worldManifestHeader, " | ") + " |\n")
	b.WriteString("|--------|------|---------|--------------|---------|-------|-------|\n")
	for _, ref := range m.Shards {
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %d | %d | %d |\n", ref.Prefix, ref.Path, ref.Version, ref.ContentHash, ref.Sources, ref.Edges, ref.Bytes)
	}
	if b.Len() > protocol.MaxBodyLength {
		return "", fmt.Errorf("world manifest %s exceeds the body limit: %d", m.World, b.Len())
	}
	return b.String(), nil
}

// ParseWorldManifest parses world's manifest. Only the bytes
// BuildWorldManifest renders for what it names are accepted.
func ParseWorldManifest(world, body string) (WorldManifest, error) {
	if len(body) > protocol.MaxBodyLength {
		return WorldManifest{}, fmt.Errorf("world manifest exceeds the body limit: %d", len(body))
	}
	meta, table, err := parseSnapshotDocument(body, worldManifestTitle)
	if err != nil {
		return WorldManifest{}, fmt.Errorf("world manifest %s: %w", world, err)
	}
	m := WorldManifest{World: world}
	var cursorErr, completeErr, lengthErr, sourcesErr, edgesErr error
	m.Cursor, cursorErr = protocol.ParseCursor(meta["Cursor"])
	m.Cursor.Epoch = strings.Clone(m.Cursor.Epoch) // not a window on body
	m.Complete, completeErr = strconv.ParseBool(meta["Complete"])
	m.PrefixLength, lengthErr = snapshotPositive("Prefix-Length", meta["Prefix-Length"])
	m.Sources, sourcesErr = snapshotNonNegative("Sources", meta["Sources"])
	m.Edges, edgesErr = snapshotNonNegative("Edges", meta["Edges"])
	rows, tableErr := parseSnapshotTable(table, worldManifestHeader)
	if err := errors.Join(cursorErr, completeErr, lengthErr, sourcesErr, edgesErr, tableErr); err != nil {
		return WorldManifest{}, fmt.Errorf("world manifest %s: %w", world, err)
	}
	m.Shards = make([]WorldShardRef, 0, len(rows))
	for _, row := range rows {
		ref := WorldShardRef{Prefix: strings.Clone(row[0]), ContentHash: strings.Clone(row[3])}
		ref.Path = WorldShardPath(world, ref.Prefix)
		var versionErr, sourcesErr, edgesErr, bytesErr error
		ref.Version, versionErr = snapshotPositive("Version", row[2])
		ref.Sources, sourcesErr = snapshotNonNegative("Sources", row[4])
		ref.Edges, edgesErr = snapshotNonNegative("Edges", row[5])
		ref.Bytes, bytesErr = snapshotPositive("Bytes", row[6])
		if err := errors.Join(versionErr, sourcesErr, edgesErr, bytesErr); err != nil {
			return WorldManifest{}, fmt.Errorf("world manifest %s: %w", world, err)
		}
		m.Shards = append(m.Shards, ref)
	}
	rebuilt, err := BuildWorldManifest(m)
	if err != nil {
		return WorldManifest{}, err
	}
	if rebuilt != body {
		return WorldManifest{}, fmt.Errorf("world manifest %s is not the manifest its pins build", world)
	}
	return m, nil
}

// LoadWorld verifies world's manifest and every shard it pins, fetched at
// its pinned version. An incomplete checkpoint loads too; callers decide.
func LoadWorld(world string, manifest protocol.Response, fetchShard func(path string) (protocol.Response, error)) (WorldManifest, []WorldSource, error) {
	if unavailable(manifest) {
		return WorldManifest{}, nil, fmt.Errorf("%w: world manifest %s returned %s", ErrWorldUnavailable, world, manifest.Status)
	}
	if manifest.Status != protocol.StatusOK {
		return WorldManifest{}, nil, fmt.Errorf("world manifest %s returned %s", world, manifest.Status)
	}
	if manifest.Metadata["content-hash"] != generation.BodyHash(manifest.Body) {
		return WorldManifest{}, nil, fmt.Errorf("world manifest %s content hash mismatch", world)
	}
	m, err := ParseWorldManifest(world, manifest.Body)
	if err != nil {
		return WorldManifest{}, nil, err
	}
	sources := make([]WorldSource, 0, m.Sources)
	for _, ref := range m.Shards {
		resp, err := fetchShard(protocol.VersionPath(ref.Path, ref.Version))
		if err != nil {
			return WorldManifest{}, nil, fmt.Errorf("%w: fetch world shard %s: %w", ErrWorldUnavailable, ref.Path, err)
		}
		if unavailable(resp) {
			return WorldManifest{}, nil, fmt.Errorf("%w: world shard %s returned %s", ErrWorldUnavailable, ref.Path, resp.Status)
		}
		shard, err := VerifyWorldShard(world, ref, resp)
		if err != nil {
			return WorldManifest{}, nil, err
		}
		sources = append(sources, shard...)
	}
	return m, sources, nil
}

// unavailable is a status that says nothing about the document: retry later.
func unavailable(resp protocol.Response) bool { //nolint:gocritic // a response is read once
	return resp.Status != protocol.StatusOK && resp.Status != protocol.StatusNotFound
}

// validateWorldSources holds built rows to the shard's rules: sorted, unique,
// canonical, and every source under prefix. It returns the edge total.
func validateWorldSources(prefix string, rows []WorldSource) (int, error) {
	edges := 0
	for i := range rows {
		row := &rows[i]
		if protocol.ValidateRequestPath(row.Path) != nil || storefmt.CanonicalPath(row.Path) != row.Path {
			return 0, fmt.Errorf("source path %q is not canonical", row.Path)
		}
		if i > 0 && rows[i-1].Path >= row.Path {
			return 0, fmt.Errorf("source %q is repeated", row.Path)
		}
		if SourcePrefix(row.Path, len(prefix)) != prefix {
			return 0, fmt.Errorf("source %q does not belong under prefix %s", row.Path, prefix)
		}
		if row.Version < 1 || !protocol.IsValidMetaValue(row.Etag) || !protocol.IsValidMetaValue(row.Title) {
			return 0, fmt.Errorf("source %q: invalid version, etag or title", row.Path)
		}
		for j := range row.Edges {
			if j > 0 && compareWorldEdges(row.Edges[j-1], row.Edges[j]) == 0 {
				return 0, fmt.Errorf("source %q: edge to %s is repeated", row.Path, row.Edges[j].To)
			}
		}
		edges += len(row.Edges)
	}
	return edges, nil
}

// compareWorldEdges orders a source's edges by identity, {To, Rel}.
func compareWorldEdges(a, b WorldEdge) int {
	if order := strings.Compare(a.To, b.To); order != 0 {
		return order
	}
	return strings.Compare(a.Rel, b.Rel)
}

func validateWorldManifest(m *WorldManifest) error {
	if !protocol.IsWorldName(m.World) {
		return fmt.Errorf("invalid world name %q", m.World)
	}
	if _, err := protocol.ParseCursor(m.Cursor.String()); err != nil {
		return fmt.Errorf("world manifest %s: cursor: %w", m.World, err)
	}
	if m.Sources > MaxSnapshotNodes || m.Edges > MaxSnapshotEdges {
		return fmt.Errorf("world manifest %s exceeds the checkpoint limits", m.World)
	}
	for i := range m.Shards {
		ref := &m.Shards[i]
		if err := validateWorldPrefix(ref.Prefix, m.PrefixLength); err != nil {
			return fmt.Errorf("world manifest %s: %w", m.World, err)
		}
		if i > 0 && m.Shards[i-1].Prefix == ref.Prefix {
			return fmt.Errorf("world manifest %s: prefix %s is repeated", m.World, ref.Prefix)
		}
		if ref.Path != WorldShardPath(m.World, ref.Prefix) {
			return fmt.Errorf("world manifest %s: shard %s is not at %s", m.World, ref.Prefix, WorldShardPath(m.World, ref.Prefix))
		}
		if hash, ok := protocol.IsHashPath(ref.ContentHash); !ok || hash != ref.ContentHash || ref.Version < 1 || ref.Sources < 1 || ref.Edges < 0 || ref.Bytes < 1 || ref.Bytes > protocol.MaxBodyLength {
			return fmt.Errorf("world manifest %s: shard %s has an invalid pin", m.World, ref.Prefix)
		}
	}
	return nil
}

// validateWorldPrefix accepts length lowercase hex digits, 1 to the maximum.
func validateWorldPrefix(prefix string, length int) error {
	if length < 1 || length > MaxWorldPrefixLength || len(prefix) != length {
		return fmt.Errorf("prefix %q is not %d hex digits (1 to %d)", prefix, length, MaxWorldPrefixLength)
	}
	for _, r := range prefix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("prefix %q is not lowercase hex", prefix)
		}
	}
	return nil
}
