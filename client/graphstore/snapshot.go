package graphstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

const (
	// SnapshotManifestPath is the additive entry point for atomic graph snapshots.
	SnapshotManifestPath = "/graph/manifest.md"
	// SnapshotManifestFormat identifies the manifest wire format.
	SnapshotManifestFormat = "demarkus-graph-snapshot/v1"
	// SnapshotShardFormat identifies the shard wire format.
	SnapshotShardFormat = "demarkus-graph-snapshot-shard/v1"
	// DefaultSnapshotShardBytes is the target shard body size.
	DefaultSnapshotShardBytes = 768 * 1024
	// MaxSnapshotShards bounds manifest fan-out.
	MaxSnapshotShards = 4096
	// MaxSnapshotNodes bounds one complete generation.
	MaxSnapshotNodes = 1_000_000
	// MaxSnapshotEdges bounds one complete generation.
	MaxSnapshotEdges = 1_000_000
	// SnapshotSlotA is the first alternating publication slot.
	SnapshotSlotA = "a"
	// SnapshotSlotB is the second alternating publication slot.
	SnapshotSlotB          = "b"
	snapshotShardKindNodes = "nodes"
	snapshotShardKindEdges = "edges"
	snapshotJSONFence      = "\n```json\n"
	snapshotJSONFenceEnd   = "\n```\n"
)

// SnapshotManifest identifies one complete immutable graph generation.
type SnapshotManifest struct {
	Exported   time.Time
	Complete   bool
	Nodes      int
	Edges      int
	ActiveSlot string
	Shards     []SnapshotShardRef
}

// SnapshotShardRef describes one immutable graph shard.
type SnapshotShardRef struct {
	Kind        string
	Part        int
	Path        string
	Version     int
	ContentHash string
	Nodes       int
	Edges       int
	Bytes       int
}

// SnapshotArtifact is a graph shard before its published version is known.
type SnapshotArtifact struct {
	Kind        string
	Part        int
	Path        string
	Body        string
	ContentHash string
	Nodes       int
	Edges       int
	Bytes       int
}

// Ref returns the immutable descriptor for a published artifact.
func (a SnapshotArtifact) Ref(version int) SnapshotShardRef { //nolint:gocritic // immutable descriptor conversion
	return SnapshotShardRef{
		Kind: a.Kind, Part: a.Part, Path: a.Path, Version: version,
		ContentHash: a.ContentHash, Nodes: a.Nodes, Edges: a.Edges, Bytes: a.Bytes,
	}
}

// InactiveSnapshotSlot returns the slot not named by the current manifest.
func InactiveSnapshotSlot(active string) string {
	if active == SnapshotSlotA {
		return SnapshotSlotB
	}
	return SnapshotSlotA
}

// SnapshotVersionPath returns an immutable FETCH path.
func SnapshotVersionPath(docPath string, version int) string {
	return fmt.Sprintf("%s/v%d", docPath, version)
}

// SnapshotBodyHash returns the protocol content hash for body.
func SnapshotBodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return fmt.Sprintf("sha256-%x", sum)
}

// BuildSnapshotManifest renders a deterministic snapshot manifest.
func BuildSnapshotManifest(manifestPath string, manifest SnapshotManifest) (string, error) { //nolint:gocritic // local copy is sorted without mutating the caller
	manifest.Shards = slices.Clone(manifest.Shards)
	slices.SortFunc(manifest.Shards, compareSnapshotRefs)
	if err := validateSnapshotManifest(manifestPath, manifest); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Graph Snapshot Manifest\n\n")
	fmt.Fprintf(&b, "> Format: %s\n", SnapshotManifestFormat)
	fmt.Fprintf(&b, "> Exported: %s\n", manifest.Exported.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "> Complete: %t\n", manifest.Complete)
	fmt.Fprintf(&b, "> Nodes: %d\n", manifest.Nodes)
	fmt.Fprintf(&b, "> Edges: %d\n", manifest.Edges)
	fmt.Fprintf(&b, "> Active-Slot: %s\n\n", manifest.ActiveSlot)
	b.WriteString("| Kind | Part | Path | Version | Content Hash | Nodes | Edges | Bytes |\n")
	b.WriteString("|------|------|------|---------|--------------|-------|-------|-------|\n")
	for _, ref := range manifest.Shards {
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %s | %d | %d | %d |\n",
			ref.Kind, ref.Part, ref.Path, ref.Version, ref.ContentHash, ref.Nodes, ref.Edges, ref.Bytes)
	}
	if b.Len() > protocol.MaxBodyLength {
		return "", fmt.Errorf("snapshot manifest exceeds body limit: %d", b.Len())
	}
	return b.String(), nil
}

// ParseSnapshotManifest strictly parses a snapshot manifest.
func ParseSnapshotManifest(manifestPath, body string) (SnapshotManifest, error) {
	if len(body) > protocol.MaxBodyLength {
		return SnapshotManifest{}, fmt.Errorf("snapshot manifest exceeds body limit: %d", len(body))
	}
	meta, table, err := parseSnapshotDocument(body, "# Graph Snapshot Manifest")
	if err != nil {
		return SnapshotManifest{}, err
	}
	if err := validateSnapshotMetadataKeys(meta, "Format", "Exported", "Complete", "Nodes", "Edges", "Active-Slot"); err != nil {
		return SnapshotManifest{}, err
	}
	if meta["Format"] != SnapshotManifestFormat {
		return SnapshotManifest{}, fmt.Errorf("unsupported graph snapshot format %q", meta["Format"])
	}
	exported, err := time.Parse(time.RFC3339, meta["Exported"])
	if err != nil {
		return SnapshotManifest{}, fmt.Errorf("invalid Exported value: %w", err)
	}
	if exported.Year() < 1 || exported.Year() > 9999 {
		return SnapshotManifest{}, errors.New("invalid Exported year")
	}
	complete, err := strconv.ParseBool(meta["Complete"])
	if err != nil {
		return SnapshotManifest{}, fmt.Errorf("invalid Complete value: %w", err)
	}
	nodeCount, err := snapshotNonNegative("Nodes", meta["Nodes"])
	if err != nil {
		return SnapshotManifest{}, err
	}
	edgeCount, err := snapshotNonNegative("Edges", meta["Edges"])
	if err != nil {
		return SnapshotManifest{}, err
	}
	rows, err := parseSnapshotTable(table, []string{"Kind", "Part", "Path", "Version", "Content Hash", "Nodes", "Edges", "Bytes"})
	if err != nil {
		return SnapshotManifest{}, err
	}
	refs := make([]SnapshotShardRef, 0, len(rows))
	for rowIndex, row := range rows {
		part, partErr := snapshotNonNegative("Part", row[1])
		version, versionErr := snapshotPositive("Version", row[3])
		nodes, nodesErr := snapshotNonNegative("Nodes", row[5])
		edges, edgesErr := snapshotNonNegative("Edges", row[6])
		bytes, bytesErr := snapshotPositive("Bytes", row[7])
		if err := errors.Join(partErr, versionErr, nodesErr, edgesErr, bytesErr); err != nil {
			return SnapshotManifest{}, fmt.Errorf("shard row %d: %w", rowIndex+1, err)
		}
		refs = append(refs, SnapshotShardRef{
			Kind: row[0], Part: part, Path: row[2], Version: version,
			ContentHash: row[4], Nodes: nodes, Edges: edges, Bytes: bytes,
		})
	}
	manifest := SnapshotManifest{
		Exported: exported, Complete: complete, Nodes: nodeCount, Edges: edgeCount,
		ActiveSlot: meta["Active-Slot"], Shards: refs,
	}
	if err := validateSnapshotManifest(manifestPath, manifest); err != nil {
		return SnapshotManifest{}, err
	}
	return manifest, nil
}

// BuildSnapshotShards partitions a graph into bounded node and edge shards.
func BuildSnapshotShards(manifestPath, slot string, nodes []StoredNode, edges []StoredEdge, targetBytes int) ([]SnapshotArtifact, error) {
	if err := validateSnapshotManifestPath(manifestPath); err != nil {
		return nil, err
	}
	if slot != SnapshotSlotA && slot != SnapshotSlotB {
		return nil, fmt.Errorf("invalid snapshot slot %q", slot)
	}
	if targetBytes == 0 {
		targetBytes = DefaultSnapshotShardBytes
	}
	if targetBytes < 1 || targetBytes > protocol.MaxBodyLength {
		return nil, fmt.Errorf("invalid snapshot shard target %d", targetBytes)
	}
	if len(nodes) > MaxSnapshotNodes || len(edges) > MaxSnapshotEdges {
		return nil, fmt.Errorf("graph exceeds snapshot limits: %d nodes, %d edges", len(nodes), len(edges))
	}
	nodes = append([]StoredNode(nil), nodes...)
	edges = append([]StoredEdge(nil), edges...)
	seenNodes := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		if err := validateSnapshotURL(nodes[i].URL); err != nil {
			return nil, err
		}
		nodes[i].URL = links.CanonicalURL(nodes[i].URL)
		if _, duplicate := seenNodes[nodes[i].URL]; duplicate {
			return nil, fmt.Errorf("duplicate graph node %q", nodes[i].URL)
		}
		seenNodes[nodes[i].URL] = struct{}{}
	}
	seenEdges := make(map[edgeKey]struct{}, len(edges))
	for i := range edges {
		if err := validateSnapshotURL(edges[i].From); err != nil {
			return nil, err
		}
		if err := validateSnapshotURL(edges[i].To); err != nil {
			return nil, err
		}
		edges[i].From = links.CanonicalURL(edges[i].From)
		edges[i].To = links.CanonicalURL(edges[i].To)
		if _, duplicate := seenEdges[edges[i].key()]; duplicate {
			return nil, fmt.Errorf("duplicate graph edge %q -> %q", edges[i].From, edges[i].To)
		}
		seenEdges[edges[i].key()] = struct{}{}
	}
	slices.SortFunc(nodes, func(a, b StoredNode) int { return strings.Compare(a.URL, b.URL) })
	slices.SortFunc(edges, compareSnapshotEdges)
	artifacts, err := buildSnapshotNodeShards(manifestPath, slot, nodes, targetBytes)
	if err != nil {
		return nil, err
	}
	edgeArtifacts, err := buildSnapshotEdgeShards(manifestPath, slot, edges, targetBytes)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, edgeArtifacts...)
	if len(artifacts) > MaxSnapshotShards {
		return nil, fmt.Errorf("snapshot has %d shards, limit is %d", len(artifacts), MaxSnapshotShards)
	}
	return artifacts, nil
}

// LoadSnapshot verifies and combines every shard in one complete generation.
func LoadSnapshot(manifestPath string, response protocol.Response, fetchShard func(string) (protocol.Response, error)) ([]StoredNode, []StoredEdge, error) {
	if response.Status != protocol.StatusOK {
		return nil, nil, fmt.Errorf("snapshot manifest returned %s", response.Status)
	}
	if response.Metadata["content-hash"] != SnapshotBodyHash(response.Body) {
		return nil, nil, errors.New("snapshot manifest content hash mismatch")
	}
	manifest, err := ParseSnapshotManifest(manifestPath, response.Body)
	if err != nil {
		return nil, nil, err
	}
	if !manifest.Complete {
		return nil, nil, errors.New("graph snapshot is incomplete")
	}
	var nodes []StoredNode
	var edges []StoredEdge
	seenNodes := make(map[string]struct{}, manifest.Nodes)
	seenEdges := make(map[edgeKey]struct{}, manifest.Edges)
	for _, ref := range manifest.Shards {
		shard, err := fetchShard(SnapshotVersionPath(ref.Path, ref.Version))
		if err != nil {
			return nil, nil, fmt.Errorf("fetch graph shard %s: %w", ref.Path, err)
		}
		shardNodes, shardEdges, err := verifySnapshotShard(ref, shard)
		if err != nil {
			return nil, nil, err
		}
		for _, node := range shardNodes {
			if _, duplicate := seenNodes[node.URL]; duplicate {
				return nil, nil, fmt.Errorf("duplicate graph snapshot node %q", node.URL)
			}
			seenNodes[node.URL] = struct{}{}
			nodes = append(nodes, node)
		}
		for _, edge := range shardEdges {
			if _, duplicate := seenEdges[edge.key()]; duplicate {
				return nil, nil, fmt.Errorf("duplicate graph snapshot edge %q -> %q", edge.From, edge.To)
			}
			seenEdges[edge.key()] = struct{}{}
			edges = append(edges, edge)
		}
	}
	if len(nodes) != manifest.Nodes || len(edges) != manifest.Edges {
		return nil, nil, fmt.Errorf("snapshot count mismatch: got %d nodes/%d edges, want %d/%d",
			len(nodes), len(edges), manifest.Nodes, manifest.Edges)
	}
	return nodes, edges, nil
}

type snapshotNode struct {
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	LinkCount int    `json:"links"`
}

type snapshotEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Rel    string `json:"rel,omitempty"`
	Label  string `json:"label,omitempty"`
	Anchor string `json:"anchor,omitempty"`
	Count  int    `json:"count"`
}

type snapshotPayload struct {
	Nodes []snapshotNode `json:"nodes,omitempty"`
	Edges []snapshotEdge `json:"edges,omitempty"`
}

func buildSnapshotNodeShards(manifestPath, slot string, nodes []StoredNode, target int) ([]SnapshotArtifact, error) {
	var artifacts []SnapshotArtifact
	for start := 0; start < len(nodes); {
		part := len(artifacts)
		end, artifact, err := largestSnapshotNodeShard(manifestPath, slot, nodes, start, part, target)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
		start = end
	}
	return artifacts, nil
}

func buildSnapshotEdgeShards(manifestPath, slot string, edges []StoredEdge, target int) ([]SnapshotArtifact, error) {
	var artifacts []SnapshotArtifact
	for start := 0; start < len(edges); {
		part := len(artifacts)
		end, artifact, err := largestSnapshotEdgeShard(manifestPath, slot, edges, start, part, target)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
		start = end
	}
	return artifacts, nil
}

func largestSnapshotNodeShard(manifestPath, slot string, nodes []StoredNode, start, part, target int) (int, SnapshotArtifact, error) {
	bestEnd := start + 1
	best, err := buildSnapshotArtifact(manifestPath, slot, snapshotShardKindNodes, part, nodes[start:bestEnd], nil)
	if err != nil {
		return 0, SnapshotArtifact{}, err
	}
	if best.Bytes > target {
		return 0, SnapshotArtifact{}, fmt.Errorf("one graph node exceeds shard target %d", target)
	}
	high := bestEnd
	for bestEnd < len(nodes) {
		probeEnd := min(start+2*(bestEnd-start), len(nodes))
		artifact, buildErr := buildSnapshotArtifact(manifestPath, slot, snapshotShardKindNodes, part, nodes[start:probeEnd], nil)
		if buildErr != nil {
			return 0, SnapshotArtifact{}, buildErr
		}
		if artifact.Bytes > target {
			high = probeEnd - 1
			break
		}
		bestEnd, best = probeEnd, artifact
	}
	for low := bestEnd + 1; low <= high; {
		mid := low + (high-low)/2
		artifact, buildErr := buildSnapshotArtifact(manifestPath, slot, snapshotShardKindNodes, part, nodes[start:mid], nil)
		if buildErr != nil {
			return 0, SnapshotArtifact{}, buildErr
		}
		if artifact.Bytes <= target {
			bestEnd, best, low = mid, artifact, mid+1
		} else {
			high = mid - 1
		}
	}
	return bestEnd, best, nil
}

func largestSnapshotEdgeShard(manifestPath, slot string, edges []StoredEdge, start, part, target int) (int, SnapshotArtifact, error) {
	bestEnd := start + 1
	best, err := buildSnapshotArtifact(manifestPath, slot, snapshotShardKindEdges, part, nil, edges[start:bestEnd])
	if err != nil {
		return 0, SnapshotArtifact{}, err
	}
	if best.Bytes > target {
		return 0, SnapshotArtifact{}, fmt.Errorf("one graph edge exceeds shard target %d", target)
	}
	high := bestEnd
	for bestEnd < len(edges) {
		probeEnd := min(start+2*(bestEnd-start), len(edges))
		artifact, buildErr := buildSnapshotArtifact(manifestPath, slot, snapshotShardKindEdges, part, nil, edges[start:probeEnd])
		if buildErr != nil {
			return 0, SnapshotArtifact{}, buildErr
		}
		if artifact.Bytes > target {
			high = probeEnd - 1
			break
		}
		bestEnd, best = probeEnd, artifact
	}
	for low := bestEnd + 1; low <= high; {
		mid := low + (high-low)/2
		artifact, buildErr := buildSnapshotArtifact(manifestPath, slot, snapshotShardKindEdges, part, nil, edges[start:mid])
		if buildErr != nil {
			return 0, SnapshotArtifact{}, buildErr
		}
		if artifact.Bytes <= target {
			bestEnd, best, low = mid, artifact, mid+1
		} else {
			high = mid - 1
		}
	}
	return bestEnd, best, nil
}

func buildSnapshotArtifact(manifestPath, slot, kind string, part int, nodes []StoredNode, edges []StoredEdge) (SnapshotArtifact, error) {
	payload := snapshotPayload{Nodes: make([]snapshotNode, len(nodes)), Edges: make([]snapshotEdge, len(edges))}
	for i, node := range nodes {
		if err := validateSnapshotURL(node.URL); err != nil {
			return SnapshotArtifact{}, fmt.Errorf("node %d: %w", i+1, err)
		}
		payload.Nodes[i] = snapshotNode{URL: node.URL, Title: node.Title, Status: node.Status, LinkCount: node.LinkCount}
	}
	for i, edge := range edges {
		if err := validateSnapshotURL(edge.From); err != nil {
			return SnapshotArtifact{}, fmt.Errorf("edge %d from: %w", i+1, err)
		}
		if err := validateSnapshotURL(edge.To); err != nil {
			return SnapshotArtifact{}, fmt.Errorf("edge %d to: %w", i+1, err)
		}
		payload.Edges[i] = snapshotEdge{
			From: edge.From, To: edge.To, Rel: edge.Rel, Label: edge.Label,
			Anchor: edge.Anchor, Count: max(edge.Count, 1),
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return SnapshotArtifact{}, err
	}
	body := fmt.Sprintf("# Graph Snapshot Shard\n\n> Format: %s\n> Kind: %s\n> Part: %d\n> Nodes: %d\n> Edges: %d\n%s%s%s",
		SnapshotShardFormat, kind, part, len(nodes), len(edges), snapshotJSONFence, encoded, snapshotJSONFenceEnd)
	artifact := SnapshotArtifact{
		Kind: kind, Part: part, Path: snapshotShardPath(manifestPath, slot, kind, part),
		Body: body, Nodes: len(nodes), Edges: len(edges), Bytes: len(body),
	}
	artifact.ContentHash = SnapshotBodyHash(body)
	return artifact, nil
}

func verifySnapshotShard(ref SnapshotShardRef, response protocol.Response) ([]StoredNode, []StoredEdge, error) { //nolint:gocyclo,gocritic // verification keeps descriptor checks together
	if response.Status != protocol.StatusOK {
		return nil, nil, fmt.Errorf("graph shard %s returned %s", ref.Path, response.Status)
	}
	version, err := snapshotPositive("version", response.Metadata["version"])
	if err != nil {
		return nil, nil, fmt.Errorf("graph shard %s: %w", ref.Path, err)
	}
	if version != ref.Version {
		return nil, nil, fmt.Errorf("graph shard %s version mismatch", ref.Path)
	}
	if len(response.Body) != ref.Bytes || SnapshotBodyHash(response.Body) != ref.ContentHash || response.Metadata["content-hash"] != ref.ContentHash {
		return nil, nil, fmt.Errorf("graph shard %s content mismatch", ref.Path)
	}
	meta, payloadText, err := parseSnapshotDocument(response.Body, "# Graph Snapshot Shard")
	if err != nil {
		return nil, nil, fmt.Errorf("graph shard %s: %w", ref.Path, err)
	}
	if err := validateSnapshotMetadataKeys(meta, "Format", "Kind", "Part", "Nodes", "Edges"); err != nil {
		return nil, nil, fmt.Errorf("graph shard %s: %w", ref.Path, err)
	}
	if meta["Format"] != SnapshotShardFormat || meta["Kind"] != ref.Kind || meta["Part"] != strconv.Itoa(ref.Part) {
		return nil, nil, fmt.Errorf("graph shard %s identity mismatch", ref.Path)
	}
	var payload snapshotPayload
	if err := validateSnapshotJSON(payloadText); err != nil {
		return nil, nil, fmt.Errorf("graph shard %s payload: %w", ref.Path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(payloadText))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("graph shard %s payload: %w", ref.Path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("graph shard %s has trailing JSON", ref.Path)
	}
	declaredNodes, nodesErr := snapshotNonNegative("Nodes", meta["Nodes"])
	declaredEdges, edgesErr := snapshotNonNegative("Edges", meta["Edges"])
	if err := errors.Join(nodesErr, edgesErr); err != nil || declaredNodes != ref.Nodes || declaredEdges != ref.Edges {
		return nil, nil, fmt.Errorf("graph shard %s declared count mismatch", ref.Path)
	}
	if len(payload.Nodes) != ref.Nodes || len(payload.Edges) != ref.Edges {
		return nil, nil, fmt.Errorf("graph shard %s count mismatch", ref.Path)
	}
	nodes := make([]StoredNode, len(payload.Nodes))
	for i, node := range payload.Nodes {
		if err := validateSnapshotURL(node.URL); err != nil {
			return nil, nil, err
		}
		node.URL = links.CanonicalURL(node.URL)
		if node.LinkCount < 0 {
			return nil, nil, errors.New("graph shard link count must be non-negative")
		}
		nodes[i] = StoredNode{URL: node.URL, Title: node.Title, Status: node.Status, LinkCount: node.LinkCount}
	}
	edges := make([]StoredEdge, len(payload.Edges))
	for i, edge := range payload.Edges {
		if err := validateSnapshotURL(edge.From); err != nil {
			return nil, nil, err
		}
		if err := validateSnapshotURL(edge.To); err != nil {
			return nil, nil, err
		}
		edge.From = links.CanonicalURL(edge.From)
		edge.To = links.CanonicalURL(edge.To)
		if edge.Count < 1 {
			return nil, nil, errors.New("graph shard edge count must be positive")
		}
		edges[i] = StoredEdge(edge)
	}
	return nodes, edges, nil
}

func validateSnapshotManifest(manifestPath string, manifest SnapshotManifest) error { //nolint:gocyclo,gocritic // one pass enforces descriptor invariants
	if err := validateSnapshotManifestPath(manifestPath); err != nil {
		return err
	}
	if manifest.Exported.IsZero() || manifest.Exported.Year() < 1 || manifest.Exported.Year() > 9999 || !manifest.Complete {
		return errors.New("complete snapshot metadata is required")
	}
	if manifest.ActiveSlot != SnapshotSlotA && manifest.ActiveSlot != SnapshotSlotB {
		return fmt.Errorf("invalid snapshot slot %q", manifest.ActiveSlot)
	}
	if manifest.Nodes < 0 || manifest.Nodes > MaxSnapshotNodes || manifest.Edges < 0 || manifest.Edges > MaxSnapshotEdges {
		return errors.New("snapshot counts exceed limits")
	}
	if len(manifest.Shards) > MaxSnapshotShards {
		return fmt.Errorf("snapshot has %d shards, limit is %d", len(manifest.Shards), MaxSnapshotShards)
	}
	counts := map[string]int{snapshotShardKindNodes: 0, snapshotShardKindEdges: 0}
	parts := map[string]map[int]struct{}{snapshotShardKindNodes: {}, snapshotShardKindEdges: {}}
	seen := make(map[string]struct{}, len(manifest.Shards))
	for _, ref := range manifest.Shards {
		if (ref.Kind != snapshotShardKindNodes && ref.Kind != snapshotShardKindEdges) || ref.Part < 0 || ref.Version < 1 || ref.Bytes < 1 || ref.Bytes > protocol.MaxBodyLength ||
			ref.Nodes > MaxSnapshotNodes || ref.Edges > MaxSnapshotEdges {
			return fmt.Errorf("invalid graph shard descriptor %q", ref.Path)
		}
		if ref.Kind == snapshotShardKindNodes && (ref.Nodes < 1 || ref.Edges != 0) {
			return fmt.Errorf("invalid node shard counts for %q", ref.Path)
		}
		if ref.Kind == snapshotShardKindEdges && (ref.Edges < 1 || ref.Nodes != 0) {
			return fmt.Errorf("invalid edge shard counts for %q", ref.Path)
		}
		if clean, ok := protocol.IsHashPath(ref.ContentHash); !ok || clean != ref.ContentHash {
			return fmt.Errorf("invalid graph shard hash %q", ref.ContentHash)
		}
		if ref.Path != snapshotShardPath(manifestPath, manifest.ActiveSlot, ref.Kind, ref.Part) {
			return fmt.Errorf("invalid graph shard path %q", ref.Path)
		}
		if _, duplicate := seen[ref.Path]; duplicate {
			return fmt.Errorf("duplicate graph shard path %q", ref.Path)
		}
		seen[ref.Path] = struct{}{}
		parts[ref.Kind][ref.Part] = struct{}{}
		counts[snapshotShardKindNodes] += ref.Nodes
		counts[snapshotShardKindEdges] += ref.Edges
	}
	for kind, kindParts := range parts {
		for part := range len(kindParts) {
			if _, ok := kindParts[part]; !ok {
				return fmt.Errorf("graph %s shards are missing part %d", kind, part)
			}
		}
	}
	if counts[snapshotShardKindNodes] != manifest.Nodes || counts[snapshotShardKindEdges] != manifest.Edges {
		return errors.New("snapshot manifest counts do not match shard rows")
	}
	return nil
}

func validateSnapshotManifestPath(manifestPath string) error {
	if err := protocol.ValidateRequestPath(manifestPath); err != nil || path.Clean(manifestPath) != manifestPath || manifestPath == "/" || !strings.HasSuffix(manifestPath, ".md") || strings.Contains(manifestPath, "|") {
		return fmt.Errorf("invalid snapshot manifest path %q", manifestPath)
	}
	if _, ok := protocol.IsHashPath(manifestPath); ok || snapshotVersionSuffix(manifestPath) {
		return fmt.Errorf("snapshot manifest path conflicts with immutable fetch syntax %q", manifestPath)
	}
	generated := snapshotShardPath(manifestPath, SnapshotSlotA, snapshotShardKindNodes, MaxSnapshotShards-1)
	if err := protocol.ValidateRequestPath(SnapshotVersionPath(generated, math.MaxInt)); err != nil {
		return fmt.Errorf("snapshot path cannot produce valid shards: %w", err)
	}
	return nil
}

func snapshotVersionSuffix(docPath string) bool {
	base := path.Base(docPath)
	if len(base) < 2 || base[0] != 'v' {
		return false
	}
	version, err := strconv.Atoi(base[1:])
	return err == nil && version > 0
}

func snapshotShardPath(manifestPath, slot, kind string, part int) string {
	return fmt.Sprintf("%s/%s/%s-%03d.md", SnapshotShardRoot(manifestPath), slot, kind, part)
}

// SnapshotShardRoot returns the generated shard directory for a manifest.
func SnapshotShardRoot(manifestPath string) string {
	root := strings.TrimSuffix(manifestPath, ".md") + ".shards"
	if path.Base(manifestPath) == "manifest.md" {
		root = path.Join(path.Dir(manifestPath), "shards")
	}
	return root
}

func parseSnapshotDocument(body, title string) (metadata map[string]string, table string, err error) {
	if !strings.HasPrefix(body, title+"\n") {
		return nil, "", fmt.Errorf("expected title %q", title)
	}
	if strings.Contains(body, snapshotJSONFence) {
		head, rest, ok := strings.Cut(body, snapshotJSONFence)
		if !ok {
			return nil, "", errors.New("snapshot JSON fence is missing")
		}
		payload, suffix, ok := strings.Cut(rest, snapshotJSONFenceEnd)
		if !ok || suffix != "" {
			return nil, "", errors.New("snapshot JSON fence is malformed")
		}
		meta, err := parseSnapshotMetadata(head)
		return meta, payload, err
	}
	tableAt := strings.Index(body, "\n|")
	if tableAt < 0 {
		return nil, "", errors.New("snapshot table is missing")
	}
	meta, err := parseSnapshotMetadata(body[:tableAt])
	if err != nil {
		return nil, "", err
	}
	return meta, body[tableAt+1:], nil
}

func parseSnapshotMetadata(body string) (map[string]string, error) {
	meta := make(map[string]string)
	for lineNumber, line := range strings.Split(body, "\n") {
		if lineNumber == 0 && strings.HasPrefix(line, "# ") {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "> ") {
			return nil, fmt.Errorf("unexpected snapshot content %q", line)
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "> "), ": ")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("invalid snapshot metadata %q", line)
		}
		if _, duplicate := meta[key]; duplicate {
			return nil, fmt.Errorf("duplicate snapshot metadata %q", key)
		}
		meta[key] = value
	}
	return meta, nil
}

func validateSnapshotJSON(payload string) error {
	decoder := json.NewDecoder(strings.NewReader(payload))
	if err := consumeSnapshotJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

func validateSnapshotMetadataKeys(meta map[string]string, allowed ...string) error {
	want := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		want[key] = struct{}{}
	}
	for key := range meta {
		if _, ok := want[key]; !ok {
			return fmt.Errorf("unexpected snapshot metadata %q", key)
		}
	}
	for key := range want {
		if _, ok := meta[key]; !ok {
			return fmt.Errorf("missing snapshot metadata %q", key)
		}
	}
	return nil
}

func consumeSnapshotJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeSnapshotJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeSnapshotJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func parseSnapshotTable(table string, header []string) ([][]string, error) {
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) < 2 {
		return nil, errors.New("snapshot table is incomplete")
	}
	parseRow := func(line string) ([]string, bool) {
		line = strings.TrimSpace(line)
		if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
			return nil, false
		}
		cells := strings.Split(line[1:len(line)-1], "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		return cells, true
	}
	gotHeader, ok := parseRow(lines[0])
	if !ok || !slices.Equal(gotHeader, header) {
		return nil, errors.New("snapshot table header is invalid")
	}
	separator, ok := parseRow(lines[1])
	if !ok || len(separator) != len(header) {
		return nil, errors.New("snapshot table separator is invalid")
	}
	for _, cell := range separator {
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return nil, errors.New("snapshot table separator is invalid")
		}
	}
	rows := make([][]string, 0, len(lines)-2)
	for _, line := range lines[2:] {
		row, ok := parseRow(line)
		if !ok || len(row) != len(header) {
			return nil, errors.New("snapshot table row is invalid")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func snapshotPositive(name, raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("invalid %s value %q", name, raw)
	}
	return value, nil
}

func snapshotNonNegative(name, raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid %s value %q", name, raw)
	}
	return value, nil
}

func validateSnapshotURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || strings.ContainsAny(raw, "\r\n\t") {
		return fmt.Errorf("invalid graph URL %q", raw)
	}
	if parsed.Scheme == "mark" {
		if parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("invalid graph URL %q", raw)
		}
	}
	return nil
}

func compareSnapshotRefs(a, b SnapshotShardRef) int { //nolint:gocritic // slices.SortFunc signature
	if order := strings.Compare(a.Kind, b.Kind); order != 0 {
		return order
	}
	return a.Part - b.Part
}

func compareSnapshotEdges(a, b StoredEdge) int { //nolint:gocritic // slices.SortFunc signature
	if order := strings.Compare(a.From, b.From); order != 0 {
		return order
	}
	if order := strings.Compare(a.To, b.To); order != 0 {
		return order
	}
	return strings.Compare(a.Rel, b.Rel)
}
