package graphstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// A world checkpoint is a manifest plus shards that partition the world's
// sources by a prefix of sha256(path), so one changed document rewrites one
// shard. Bodies are deterministic and carry no wall-clock field.
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

var worldManifestHeader = []string{"Prefix", "Path", "Version", "Content Hash", "Sources", "Edges", "Bytes"}

// worldNameRE is the broker's world name rule, a DNS label.
var worldNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// WorldSource is one document of a world as derived at Version: its node and
// the edges it links out with. Links counts body links, the node's LinkCount.
type WorldSource struct {
	Path    string
	Version int
	Etag    string
	Title   string
	Links   int
	Edges   []WorldEdge
}

// WorldEdge is one outgoing edge of a source, which is its origin.
type WorldEdge struct {
	To, Rel, Label, Anchor string
	Count                  int
}

// WorldManifest names one world's checkpoint: every shard pinned at its
// version, and the change-feed cursor the shards are complete through.
type WorldManifest struct {
	World        string
	Cursor       protocol.Cursor
	Derived      time.Time
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

// WorldShard is a built shard before its published version is known.
type WorldShard struct {
	Prefix, Path, Body, ContentHash string
	Sources, Edges                  int
}

// Ref pins the shard at its published version.
func (s *WorldShard) Ref(version int) WorldShardRef {
	return WorldShardRef{Prefix: s.Prefix, Path: s.Path, Version: version, ContentHash: s.ContentHash, Sources: s.Sources, Edges: s.Edges, Bytes: len(s.Body)}
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

type worldEdgeJSON struct {
	To     string `json:"to"`
	Rel    string `json:"rel,omitempty"`
	Label  string `json:"label,omitempty"`
	Anchor string `json:"anchor,omitempty"`
	Count  int    `json:"count"`
}

type worldSourceJSON struct {
	Path    string          `json:"path"`
	Version int             `json:"version"`
	Etag    string          `json:"etag,omitempty"`
	Title   string          `json:"title,omitempty"`
	Links   int             `json:"links"`
	Edges   []worldEdgeJSON `json:"edges,omitempty"`
}

type worldShardJSON struct {
	Sources []worldSourceJSON `json:"sources"`
}

// BuildWorldShard renders the shard of world's sources under prefix, sorted
// so equal sources always render equal bytes.
func BuildWorldShard(world, prefix string, sources []WorldSource) (WorldShard, error) {
	if err := validateWorldName(world); err != nil {
		return WorldShard{}, err
	}
	if err := validateWorldPrefix(prefix, len(prefix)); err != nil {
		return WorldShard{}, err
	}
	payload := worldShardJSON{Sources: make([]worldSourceJSON, len(sources))}
	edges := 0
	for i := range sources {
		src := &sources[i]
		row := worldSourceJSON{Path: src.Path, Version: src.Version, Etag: src.Etag, Title: src.Title, Links: src.Links, Edges: make([]worldEdgeJSON, len(src.Edges))}
		for j, edge := range src.Edges {
			row.Edges[j] = worldEdgeJSON{To: links.CanonicalURL(edge.To), Rel: edge.Rel, Label: edge.Label, Anchor: edge.Anchor, Count: max(edge.Count, 1)}
		}
		slices.SortFunc(row.Edges, compareWorldEdges)
		payload.Sources[i] = row
		edges += len(row.Edges)
	}
	slices.SortFunc(payload.Sources, func(a, b worldSourceJSON) int { return strings.Compare(a.Path, b.Path) })
	if err := validateWorldSources(prefix, payload.Sources); err != nil {
		return WorldShard{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return WorldShard{}, fmt.Errorf("encode world shard %s/%s: %w", world, prefix, err)
	}
	body := fmt.Sprintf("%s\n\n> Format: %s\n> World: %s\n> Prefix: %s\n> Sources: %d\n> Edges: %d\n%s%s%s",
		worldShardTitle, WorldShardFormat, world, prefix, len(payload.Sources), edges, snapshotJSONFence, encoded, snapshotJSONFenceEnd)
	if len(body) > protocol.MaxBodyLength {
		return WorldShard{}, fmt.Errorf("world shard %s/%s is %d bytes, over the body limit: lengthen the prefix", world, prefix, len(body))
	}
	return WorldShard{Prefix: prefix, Path: WorldShardPath(world, prefix), Body: body, ContentHash: generation.BodyHash(body), Sources: len(payload.Sources), Edges: edges}, nil
}

// BuildWorldManifest renders m with its shards in prefix order.
func BuildWorldManifest(m WorldManifest) (string, error) { //nolint:gocritic // local copy is sorted without mutating the caller
	m.Shards = slices.Clone(m.Shards)
	slices.SortFunc(m.Shards, func(a, b WorldShardRef) int { return strings.Compare(a.Prefix, b.Prefix) })
	if err := validateWorldManifest(&m); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(worldManifestTitle + "\n\n")
	fmt.Fprintf(&b, "> Format: %s\n", WorldManifestFormat)
	fmt.Fprintf(&b, "> World: %s\n", m.World)
	fmt.Fprintf(&b, "> Cursor: %s\n", m.Cursor)
	fmt.Fprintf(&b, "> Derived: %s\n", m.Derived.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "> Complete: %t\n", m.Complete)
	fmt.Fprintf(&b, "> Prefix-Length: %d\n", m.PrefixLength)
	fmt.Fprintf(&b, "> Sources: %d\n", m.Sources)
	fmt.Fprintf(&b, "> Edges: %d\n\n", m.Edges)
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

// ParseWorldManifest strictly parses world's manifest.
func ParseWorldManifest(world, body string) (WorldManifest, error) {
	if len(body) > protocol.MaxBodyLength {
		return WorldManifest{}, fmt.Errorf("world manifest exceeds the body limit: %d", len(body))
	}
	meta, table, err := parseSnapshotDocument(body, worldManifestTitle)
	if err != nil {
		return WorldManifest{}, fmt.Errorf("world manifest %s: %w", world, err)
	}
	if err := validateSnapshotMetadataKeys(meta, "Format", "World", "Cursor", "Derived", "Complete", "Prefix-Length", "Sources", "Edges"); err != nil {
		return WorldManifest{}, fmt.Errorf("world manifest %s: %w", world, err)
	}
	if meta["Format"] != WorldManifestFormat || meta["World"] != world {
		return WorldManifest{}, fmt.Errorf("world manifest %s: format %q of world %q", world, meta["Format"], meta["World"])
	}
	m := WorldManifest{World: world}
	var cursorErr, derivedErr, completeErr, lengthErr, sourcesErr, edgesErr error
	m.Cursor, cursorErr = protocol.ParseCursor(meta["Cursor"])
	m.Derived, derivedErr = time.Parse(time.RFC3339, meta["Derived"])
	m.Complete, completeErr = strconv.ParseBool(meta["Complete"])
	m.PrefixLength, lengthErr = snapshotPositive("Prefix-Length", meta["Prefix-Length"])
	m.Sources, sourcesErr = snapshotNonNegative("Sources", meta["Sources"])
	m.Edges, edgesErr = snapshotNonNegative("Edges", meta["Edges"])
	rows, tableErr := parseSnapshotTable(table, worldManifestHeader)
	if err := errors.Join(cursorErr, derivedErr, completeErr, lengthErr, sourcesErr, edgesErr, tableErr); err != nil {
		return WorldManifest{}, fmt.Errorf("world manifest %s: %w", world, err)
	}
	for _, row := range rows {
		ref, err := parseWorldShardRow(row)
		if err != nil {
			return WorldManifest{}, fmt.Errorf("world manifest %s: %w", world, err)
		}
		m.Shards = append(m.Shards, ref)
	}
	if err := validateWorldManifest(&m); err != nil {
		return WorldManifest{}, err
	}
	return m, nil
}

func parseWorldShardRow(row []string) (WorldShardRef, error) {
	ref := WorldShardRef{Prefix: row[0], Path: row[1], ContentHash: row[3]}
	var versionErr, sourcesErr, edgesErr, bytesErr error
	ref.Version, versionErr = snapshotPositive("Version", row[2])
	ref.Sources, sourcesErr = snapshotNonNegative("Sources", row[4])
	ref.Edges, edgesErr = snapshotNonNegative("Edges", row[5])
	ref.Bytes, bytesErr = snapshotPositive("Bytes", row[6])
	return ref, errors.Join(versionErr, sourcesErr, edgesErr, bytesErr)
}

// LoadWorld verifies world's manifest and every shard it pins, fetched at
// its pinned version. An incomplete checkpoint loads too; callers decide.
func LoadWorld(world string, manifest protocol.Response, fetchShard func(path string) (protocol.Response, error)) (WorldManifest, []WorldSource, error) {
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
			return WorldManifest{}, nil, fmt.Errorf("fetch world shard %s: %w", ref.Path, err)
		}
		shard, err := VerifyWorldShard(world, ref, resp)
		if err != nil {
			return WorldManifest{}, nil, err
		}
		sources = append(sources, shard...)
	}
	return m, sources, nil
}

// VerifyWorldShard checks a fetched shard against its pin and returns its
// sources.
func VerifyWorldShard(world string, ref WorldShardRef, resp protocol.Response) ([]WorldSource, error) { //nolint:gocritic // a pin is passed once per shard
	if err := validateWorldName(world); err != nil {
		return nil, err
	}
	if resp.Status != protocol.StatusOK {
		return nil, fmt.Errorf("world shard %s returned %s", ref.Path, resp.Status)
	}
	if version, err := snapshotPositive("version", resp.Metadata["version"]); err != nil || version != ref.Version {
		return nil, fmt.Errorf("world shard %s is not version %d", ref.Path, ref.Version)
	}
	if len(resp.Body) != ref.Bytes || generation.BodyHash(resp.Body) != ref.ContentHash || resp.Metadata["content-hash"] != ref.ContentHash {
		return nil, fmt.Errorf("world shard %s content mismatch", ref.Path)
	}
	meta, payloadText, err := parseSnapshotDocument(resp.Body, worldShardTitle)
	if err != nil {
		return nil, fmt.Errorf("world shard %s: %w", ref.Path, err)
	}
	if err := validateSnapshotMetadataKeys(meta, "Format", "World", "Prefix", "Sources", "Edges"); err != nil {
		return nil, fmt.Errorf("world shard %s: %w", ref.Path, err)
	}
	if meta["Format"] != WorldShardFormat || meta["World"] != world || meta["Prefix"] != ref.Prefix {
		return nil, fmt.Errorf("world shard %s identity mismatch", ref.Path)
	}
	if err := validateSnapshotJSON(payloadText); err != nil {
		return nil, fmt.Errorf("world shard %s payload: %w", ref.Path, err)
	}
	var payload worldShardJSON
	decoder := json.NewDecoder(strings.NewReader(payloadText))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("world shard %s payload: %w", ref.Path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("world shard %s has trailing JSON", ref.Path)
	}
	if err := validateWorldSources(ref.Prefix, payload.Sources); err != nil {
		return nil, fmt.Errorf("world shard %s: %w", ref.Path, err)
	}
	edges := 0
	sources := make([]WorldSource, len(payload.Sources))
	for i := range payload.Sources {
		row := &payload.Sources[i]
		src := WorldSource{Path: row.Path, Version: row.Version, Etag: row.Etag, Title: row.Title, Links: row.Links, Edges: make([]WorldEdge, len(row.Edges))}
		for j, edge := range row.Edges {
			src.Edges[j] = WorldEdge(edge)
		}
		edges += len(row.Edges)
		sources[i] = src
	}
	if meta["Sources"] != strconv.Itoa(len(sources)) || meta["Edges"] != strconv.Itoa(edges) || len(sources) != ref.Sources || edges != ref.Edges {
		return nil, fmt.Errorf("world shard %s count mismatch", ref.Path)
	}
	return sources, nil
}

// validateWorldSources holds a shard's rows to the form the builder renders:
// sorted, unique, canonical, and every source under the shard's prefix.
func validateWorldSources(prefix string, rows []worldSourceJSON) error {
	for i := range rows {
		row := &rows[i]
		if storefmt.CanonicalPath(row.Path) != row.Path || !strings.HasPrefix(row.Path, "/") || strings.ContainsAny(row.Path, "\r\n\t") {
			return fmt.Errorf("source path %q is not canonical", row.Path)
		}
		if i > 0 && rows[i-1].Path >= row.Path {
			return fmt.Errorf("source %q is out of order or repeated", row.Path)
		}
		if SourcePrefix(row.Path, len(prefix)) != prefix {
			return fmt.Errorf("source %q does not belong under prefix %s", row.Path, prefix)
		}
		if row.Version < 1 || row.Links < 0 || strings.ContainsAny(row.Etag+row.Title, "\r\n") {
			return fmt.Errorf("source %q: invalid version, links, etag or title", row.Path)
		}
		for j := range row.Edges {
			edge := &row.Edges[j]
			if err := validateWorldEdgeTarget(edge.To); err != nil {
				return fmt.Errorf("source %q: %w", row.Path, err)
			}
			if edge.Count < 1 {
				return fmt.Errorf("source %q: edge to %s has count %d", row.Path, edge.To, edge.Count)
			}
			if j > 0 && compareWorldEdges(row.Edges[j-1], *edge) >= 0 {
				return fmt.Errorf("source %q: edge to %s is out of order or repeated", row.Path, edge.To)
			}
		}
	}
	return nil
}

func validateWorldEdgeTarget(raw string) error {
	if err := validateSnapshotURL(raw); err != nil {
		return err
	}
	if !strings.HasPrefix(raw, "mark://") || links.CanonicalURL(raw) != raw {
		return fmt.Errorf("edge target %q is not a canonical mark URL", raw)
	}
	return nil
}

// compareWorldEdges orders a source's edges by identity, {To, Rel}.
func compareWorldEdges(a, b worldEdgeJSON) int { //nolint:gocritic // slices.SortFunc signature
	if order := strings.Compare(a.To, b.To); order != 0 {
		return order
	}
	return strings.Compare(a.Rel, b.Rel)
}

func validateWorldManifest(m *WorldManifest) error {
	if err := validateWorldName(m.World); err != nil {
		return err
	}
	if _, err := protocol.ParseCursor(m.Cursor.String()); err != nil {
		return fmt.Errorf("world manifest %s: cursor: %w", m.World, err)
	}
	if m.Derived.IsZero() || m.Derived.Year() > 9999 {
		return fmt.Errorf("world manifest %s: derived time %v", m.World, m.Derived)
	}
	if len(m.Shards) > MaxSnapshotShards || m.Sources > MaxSnapshotNodes || m.Edges > MaxSnapshotEdges {
		return fmt.Errorf("world manifest %s exceeds the checkpoint limits", m.World)
	}
	sources, edges := 0, 0
	for i := range m.Shards {
		ref := &m.Shards[i]
		if err := validateWorldPrefix(ref.Prefix, m.PrefixLength); err != nil {
			return fmt.Errorf("world manifest %s: %w", m.World, err)
		}
		if i > 0 && m.Shards[i-1].Prefix >= ref.Prefix {
			return fmt.Errorf("world manifest %s: prefix %s is out of order or repeated", m.World, ref.Prefix)
		}
		if ref.Path != WorldShardPath(m.World, ref.Prefix) {
			return fmt.Errorf("world manifest %s: shard %s is not at %s", m.World, ref.Prefix, WorldShardPath(m.World, ref.Prefix))
		}
		if _, ok := protocol.IsHashPath(ref.ContentHash); !ok || ref.Version < 1 || ref.Sources < 1 || ref.Edges < 0 || ref.Bytes < 1 || ref.Bytes > protocol.MaxBodyLength {
			return fmt.Errorf("world manifest %s: shard %s has an invalid pin", m.World, ref.Prefix)
		}
		sources += ref.Sources
		edges += ref.Edges
	}
	if sources != m.Sources || edges != m.Edges {
		return fmt.Errorf("world manifest %s: totals %d/%d, shards hold %d/%d", m.World, m.Sources, m.Edges, sources, edges)
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

func validateWorldName(world string) error {
	if !worldNameRE.MatchString(world) {
		return fmt.Errorf("invalid world name %q", world)
	}
	return nil
}
