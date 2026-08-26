package index

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// ManifestFormat identifies the sharded index manifest and groups its format limits.
const (
	ManifestFormat          = "demarkus-hash-index/v2"
	ShardFormat             = "demarkus-hash-index-shard/v1"
	DefaultShardTargetBytes = 768 * 1024
	HashPrefixLength        = 2
	MaxManifestShards       = 4096
	MaxIndexEntries         = 1_000_000
	SlotA                   = "a"
	SlotB                   = "b"
)

// ErrUnsupportedFormat reports a generated index format this client cannot read.
var ErrUnsupportedFormat = errors.New("unsupported hash index format")

// Manifest identifies one complete immutable set of hash-index shards.
type Manifest struct {
	Source     string
	Indexed    time.Time
	Complete   bool
	Documents  int
	ActiveSlot string
	Shards     []ShardRef
}

// ShardRef is the integrity and location record for one manifest shard.
type ShardRef struct {
	Prefix      string
	Path        string
	Version     int
	ContentHash string
	Rows        int
	Bytes       int
}

// ShardArtifact is a shard body before its published version is known.
type ShardArtifact struct {
	Prefix      string
	Part        int
	Path        string
	Body        string
	ContentHash string
	Rows        int
	Bytes       int
}

// Ref returns the published descriptor for an artifact.
func (a ShardArtifact) Ref(version int) ShardRef { //nolint:gocritic // immutable descriptor conversion
	return ShardRef{
		Prefix:      a.Prefix,
		Path:        a.Path,
		Version:     version,
		ContentHash: a.ContentHash,
		Rows:        a.Rows,
		Bytes:       a.Bytes,
	}
}

// InactiveSlot returns the slot not named by the current manifest.
func InactiveSlot(active string) string {
	if active == SlotA {
		return SlotB
	}
	return SlotA
}

// VersionPath returns the immutable FETCH path for one document version.
func VersionPath(docPath string, version int) string {
	return fmt.Sprintf("%s/v%d", docPath, version)
}

// BuildManifest renders a deterministic v2 manifest.
func BuildManifest(manifestPath string, m Manifest) (string, error) { //nolint:gocritic // local copy is sorted without mutating the caller
	refs := slices.Clone(m.Shards)
	slices.SortFunc(refs, compareShardRefs)
	m.Shards = refs
	if err := validateManifest(manifestPath, m); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Content Index Manifest\n\n")
	fmt.Fprintf(&b, "> Format: %s\n", ManifestFormat)
	fmt.Fprintf(&b, "> Source: %s\n", m.Source)
	fmt.Fprintf(&b, "> Indexed: %s\n", m.Indexed.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "> Complete: %t\n", m.Complete)
	fmt.Fprintf(&b, "> Documents: %d\n", m.Documents)
	fmt.Fprintf(&b, "> Active-Slot: %s\n\n", m.ActiveSlot)
	b.WriteString("| Prefix | Path | Version | Content Hash | Rows | Bytes |\n")
	b.WriteString("|--------|------|---------|--------------|------|-------|\n")
	for _, ref := range refs {
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %d | %d |\n",
			ref.Prefix, encodeCell(ref.Path), ref.Version, ref.ContentHash, ref.Rows, ref.Bytes)
	}
	if b.Len() > protocol.MaxBodyLength {
		return "", fmt.Errorf("manifest body exceeds limit: %d > %d", b.Len(), protocol.MaxBodyLength)
	}
	return b.String(), nil
}

// ParseManifest parses and validates a v2 manifest and its shard scope.
func ParseManifest(manifestPath, body string) (Manifest, error) {
	if len(body) > protocol.MaxBodyLength {
		return Manifest{}, fmt.Errorf("manifest body exceeds limit: %d", len(body))
	}
	meta, err := generatedMetadata(body, "# Content Index Manifest")
	if err != nil {
		return Manifest{}, err
	}
	if meta["Format"] != ManifestFormat {
		return Manifest{}, fmt.Errorf("%w: %q", ErrUnsupportedFormat, meta["Format"])
	}
	indexed, err := time.Parse(time.RFC3339, meta["Indexed"])
	if err != nil {
		return Manifest{}, fmt.Errorf("invalid Indexed value: %w", err)
	}
	complete, err := strconv.ParseBool(meta["Complete"])
	if err != nil {
		return Manifest{}, fmt.Errorf("invalid Complete value: %w", err)
	}
	documents, err := parseNonNegative("Documents", meta["Documents"])
	if err != nil {
		return Manifest{}, err
	}
	rows, err := tableRows(body, []string{"Prefix", "Path", "Version", "Content Hash", "Rows", "Bytes"})
	if err != nil {
		return Manifest{}, err
	}
	refs := make([]ShardRef, 0, len(rows))
	for i, row := range rows {
		version, err := parsePositive("Version", row[2])
		if err != nil {
			return Manifest{}, fmt.Errorf("shard row %d: %w", i+1, err)
		}
		rowCount, err := parsePositive("Rows", row[4])
		if err != nil {
			return Manifest{}, fmt.Errorf("shard row %d: %w", i+1, err)
		}
		encodedBytes, err := parsePositive("Bytes", row[5])
		if err != nil {
			return Manifest{}, fmt.Errorf("shard row %d: %w", i+1, err)
		}
		refs = append(refs, ShardRef{
			Prefix:      row[0],
			Path:        decodeCell(row[1]),
			Version:     version,
			ContentHash: row[3],
			Rows:        rowCount,
			Bytes:       encodedBytes,
		})
	}
	m := Manifest{
		Source:     meta["Source"],
		Indexed:    indexed,
		Complete:   complete,
		Documents:  documents,
		ActiveSlot: meta["Active-Slot"],
		Shards:     refs,
	}
	if err := validateManifest(manifestPath, m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// BuildShards partitions entries by hash prefix and deterministic overflow
// parts, keeping every encoded shard at or below targetBytes.
func BuildShards(manifestPath, slot string, entries []Entry, targetBytes int) ([]ShardArtifact, error) {
	if err := validateManifestPath(manifestPath); err != nil {
		return nil, err
	}
	if slot != SlotA && slot != SlotB {
		return nil, fmt.Errorf("invalid shard slot %q", slot)
	}
	if targetBytes == 0 {
		targetBytes = DefaultShardTargetBytes
	}
	if targetBytes < 1 || targetBytes > protocol.MaxBodyLength {
		return nil, fmt.Errorf("invalid shard target %d", targetBytes)
	}
	if len(entries) > MaxIndexEntries {
		return nil, fmt.Errorf("index has %d entries, limit is %d", len(entries), MaxIndexEntries)
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, compareEntries)
	var artifacts []ShardArtifact
	for start := 0; start < len(sorted); {
		prefix, err := HashPrefix(sorted[start].Hash)
		if err != nil {
			return nil, err
		}
		end := start
		for end < len(sorted) {
			entryPrefix, err := HashPrefix(sorted[end].Hash)
			if err != nil {
				return nil, err
			}
			if entryPrefix != prefix {
				break
			}
			end++
		}
		parts, err := buildPrefixParts(manifestPath, slot, prefix, sorted[start:end], targetBytes)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, parts...)
		start = end
	}
	return artifacts, nil
}

// HashPrefix returns the lowercase partition prefix for a content hash.
func HashPrefix(hash string) (string, error) {
	clean, ok := protocol.IsHashPath(hash)
	if !ok {
		return "", fmt.Errorf("invalid content hash %q", hash)
	}
	return clean[len("sha256-") : len("sha256-")+HashPrefixLength], nil
}

// VerifyShard validates a fetched shard against its manifest descriptor.
func VerifyShard(ref ShardRef, resp protocol.Response) ([]Entry, error) {
	if resp.Status != protocol.StatusOK {
		return nil, fmt.Errorf("shard %s returned %s", ref.Path, resp.Status)
	}
	version, err := parsePositive("version", resp.Metadata["version"])
	if err != nil || version != ref.Version {
		return nil, fmt.Errorf("shard %s version mismatch: got %q, want %d", ref.Path, resp.Metadata["version"], ref.Version)
	}
	if len(resp.Body) != ref.Bytes {
		return nil, fmt.Errorf("shard %s byte count mismatch: got %d, want %d", ref.Path, len(resp.Body), ref.Bytes)
	}
	bodyHash := BodyHash(resp.Body)
	if bodyHash != ref.ContentHash || resp.Metadata["content-hash"] != ref.ContentHash {
		return nil, fmt.Errorf("shard %s content hash mismatch", ref.Path)
	}
	prefix, part, entries, err := parseShard(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("shard %s: %w", ref.Path, err)
	}
	pathPrefix, pathPart, pathErr := parseShardFile(path.Base(ref.Path))
	if pathErr != nil || prefix != ref.Prefix || prefix != pathPrefix || part != pathPart || len(entries) != ref.Rows {
		return nil, fmt.Errorf("shard %s identity or row count mismatch", ref.Path)
	}
	return entries, nil
}

// EntriesForHash resolves matching rows from a legacy index or only the v2
// shards that can contain hash.
func EntriesForHash(manifestPath, body, hash string, fetchShard func(string) (protocol.Response, error)) ([]Entry, error) {
	cleanHash, ok := protocol.IsHashPath(hash)
	if !ok {
		return nil, fmt.Errorf("invalid content hash %q", hash)
	}
	hash = cleanHash
	format, err := documentFormat(body)
	if err != nil {
		return nil, err
	}
	if format == "" {
		return matchingEntries(Parse(body), hash), nil
	}
	if format != ManifestFormat {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	m, err := ParseManifest(manifestPath, body)
	if err != nil {
		return nil, err
	}
	if !m.Complete {
		return nil, errors.New("hash index manifest is incomplete")
	}
	prefix, err := HashPrefix(hash)
	if err != nil {
		return nil, err
	}
	var matches []Entry
	for _, ref := range m.Shards {
		if ref.Prefix != prefix {
			continue
		}
		resp, err := fetchShard(VersionPath(ref.Path, ref.Version))
		if err != nil {
			return nil, fmt.Errorf("fetch shard %s: %w", ref.Path, err)
		}
		entries, err := VerifyShard(ref, resp)
		if err != nil {
			return nil, err
		}
		matches = append(matches, matchingEntries(entries, hash)...)
	}
	return matches, nil
}

// LoadEntries reads and verifies every row from a legacy index or v2 manifest.
func LoadEntries(manifestPath, body string, fetchShard func(string) (protocol.Response, error)) ([]Entry, error) {
	format, err := documentFormat(body)
	if err != nil {
		return nil, err
	}
	if format == "" {
		return Parse(body), nil
	}
	if format != ManifestFormat {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	m, err := ParseManifest(manifestPath, body)
	if err != nil {
		return nil, err
	}
	if !m.Complete {
		return nil, errors.New("hash index manifest is incomplete")
	}
	var entries []Entry
	for _, ref := range m.Shards {
		resp, err := fetchShard(VersionPath(ref.Path, ref.Version))
		if err != nil {
			return nil, fmt.Errorf("fetch shard %s: %w", ref.Path, err)
		}
		shardEntries, err := VerifyShard(ref, resp)
		if err != nil {
			return nil, err
		}
		entries = append(entries, shardEntries...)
	}
	if len(entries) != m.Documents {
		return nil, fmt.Errorf("manifest document count mismatch: got %d, want %d", len(entries), m.Documents)
	}
	return entries, nil
}

// BodyHash returns the protocol content hash for a generated body.
func BodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return fmt.Sprintf("sha256-%x", sum)
}

func buildPrefixParts(manifestPath, slot, prefix string, entries []Entry, target int) ([]ShardArtifact, error) {
	var artifacts []ShardArtifact
	for start, part := 0, 0; start < len(entries); part++ {
		end := start
		rowBytes := 0
		for end < len(entries) {
			row, err := entryRow(entries[end])
			if err != nil {
				return nil, err
			}
			candidateRows := end - start + 1
			candidateBytes := len(shardHeader(prefix, part, candidateRows)) + rowBytes + len(row)
			if candidateBytes > target {
				if end == start {
					return nil, fmt.Errorf("one %s shard row exceeds target %d", prefix, target)
				}
				break
			}
			rowBytes += len(row)
			end++
		}
		body, err := buildShard(prefix, part, entries[start:end])
		if err != nil {
			return nil, err
		}
		artifact := ShardArtifact{
			Prefix: prefix,
			Part:   part,
			Path:   shardPath(manifestPath, slot, prefix, part),
			Body:   body,
			Rows:   end - start,
			Bytes:  len(body),
		}
		artifact.ContentHash = BodyHash(body)
		artifacts = append(artifacts, artifact)
		start = end
	}
	return artifacts, nil
}

func buildShard(prefix string, part int, entries []Entry) (string, error) {
	var b strings.Builder
	b.WriteString(shardHeader(prefix, part, len(entries)))
	for _, entry := range entries {
		row, err := entryRow(entry)
		if err != nil {
			return "", err
		}
		b.WriteString(row)
	}
	return b.String(), nil
}

func shardHeader(prefix string, part, rows int) string {
	return fmt.Sprintf("# Content Index Shard\n\n> Format: %s\n> Prefix: %s\n> Part: %d\n> Rows: %d\n\n| Hash | Server | Path |\n|------|--------|------|\n",
		ShardFormat, prefix, part, rows)
}

func parseShard(body string) (prefix string, part int, entries []Entry, err error) {
	if len(body) > protocol.MaxBodyLength {
		return "", 0, nil, fmt.Errorf("shard body exceeds limit: %d", len(body))
	}
	meta, err := generatedMetadata(body, "# Content Index Shard")
	if err != nil {
		return "", 0, nil, err
	}
	if meta["Format"] != ShardFormat {
		return "", 0, nil, fmt.Errorf("%w: %q", ErrUnsupportedFormat, meta["Format"])
	}
	prefix = meta["Prefix"]
	if !validPrefix(prefix) {
		return "", 0, nil, fmt.Errorf("invalid shard prefix %q", prefix)
	}
	part, err = parseNonNegative("Part", meta["Part"])
	if err != nil {
		return "", 0, nil, err
	}
	declaredRows, err := parsePositive("Rows", meta["Rows"])
	if err != nil {
		return "", 0, nil, err
	}
	if declaredRows > MaxIndexEntries {
		return "", 0, nil, fmt.Errorf("shard row count %d exceeds limit", declaredRows)
	}
	rows, err := tableRows(body, []string{"Hash", "Server", "Path"})
	if err != nil {
		return "", 0, nil, err
	}
	entries = make([]Entry, 0, len(rows))
	for _, row := range rows {
		entry := Entry{Hash: row[0], Server: decodeCell(row[1]), Path: decodeCell(row[2])}
		if err := validateEntry(entry); err != nil {
			return "", 0, nil, err
		}
		entryPrefix, err := HashPrefix(entry.Hash)
		if err != nil || entryPrefix != prefix {
			return "", 0, nil, fmt.Errorf("entry hash %q is outside prefix %s", entry.Hash, prefix)
		}
		entries = append(entries, entry)
	}
	if len(entries) != declaredRows {
		return "", 0, nil, fmt.Errorf("declared %d rows, body has %d", declaredRows, len(entries))
	}
	return prefix, part, entries, nil
}

func validateManifest(manifestPath string, m Manifest) error { //nolint:gocyclo,gocritic // one pass over an immutable value enforces cross-field invariants
	if err := validateManifestPath(manifestPath); err != nil {
		return err
	}
	if strings.TrimSpace(m.Source) == "" || m.Source != strings.TrimSpace(m.Source) || strings.ContainsAny(m.Source, "\r\n\t") {
		return errors.New("manifest source is required")
	}
	if m.Indexed.IsZero() || m.Indexed.Year() < 1 || m.Indexed.Year() > 9999 {
		return errors.New("manifest indexed time is required")
	}
	if m.ActiveSlot != SlotA && m.ActiveSlot != SlotB {
		return fmt.Errorf("invalid active slot %q", m.ActiveSlot)
	}
	if m.Documents < 0 || m.Documents > MaxIndexEntries {
		return fmt.Errorf("manifest documents %d exceeds limit", m.Documents)
	}
	if len(m.Shards) > MaxManifestShards {
		return fmt.Errorf("manifest has %d shards, limit is %d", len(m.Shards), MaxManifestShards)
	}
	rows := 0
	seenPaths := make(map[string]struct{}, len(m.Shards))
	parts := make(map[string]map[int]struct{})
	for _, ref := range m.Shards {
		if !validPrefix(ref.Prefix) || ref.Version < 1 || ref.Rows < 1 || ref.Bytes < 1 {
			return fmt.Errorf("invalid shard descriptor for %q", ref.Path)
		}
		if clean, ok := protocol.IsHashPath(ref.ContentHash); !ok || clean != ref.ContentHash {
			return fmt.Errorf("invalid shard content hash %q", ref.ContentHash)
		}
		prefix, part, err := parseShardPath(manifestPath, m.ActiveSlot, ref.Path)
		if err != nil || prefix != ref.Prefix {
			return fmt.Errorf("invalid shard path %q for prefix %s", ref.Path, ref.Prefix)
		}
		if _, duplicate := seenPaths[ref.Path]; duplicate {
			return fmt.Errorf("duplicate shard path %q", ref.Path)
		}
		seenPaths[ref.Path] = struct{}{}
		if parts[ref.Prefix] == nil {
			parts[ref.Prefix] = make(map[int]struct{})
		}
		parts[ref.Prefix][part] = struct{}{}
		if ref.Rows > math.MaxInt-rows {
			return errors.New("manifest shard row count overflows int")
		}
		rows += ref.Rows
	}
	for prefix, prefixParts := range parts {
		for part := range len(prefixParts) {
			if _, ok := prefixParts[part]; !ok {
				return fmt.Errorf("shard prefix %s is missing part %d", prefix, part)
			}
		}
	}
	if rows != m.Documents {
		return fmt.Errorf("manifest documents %d does not match shard rows %d", m.Documents, rows)
	}
	return nil
}

func tableRows(body string, wantHeader []string) ([][]string, error) {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		cells, ok := splitTableRow(line)
		if !ok {
			continue
		}
		if !slices.Equal(cells, wantHeader) {
			return nil, fmt.Errorf("unexpected generated table header %q", line)
		}
		if i+1 >= len(lines) || !validTableSeparator(lines[i+1], len(wantHeader)) {
			return nil, errors.New("table separator is missing")
		}
		var rows [][]string
		tableEnded := false
		for _, rowLine := range lines[i+2:] {
			cells, ok := splitDataTableRow(rowLine)
			if !ok {
				if strings.TrimSpace(rowLine) == "" {
					tableEnded = true
					continue
				}
				return nil, errors.New("unexpected content after generated table")
			}
			if tableEnded {
				return nil, errors.New("unexpected content after generated table")
			}
			if len(cells) != len(wantHeader) {
				return nil, fmt.Errorf("table row has %d columns, want %d", len(cells), len(wantHeader))
			}
			rows = append(rows, cells)
		}
		return rows, nil
	}
	return nil, errors.New("required table is missing")
}

func splitTableRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
		return nil, false
	}
	raw := strings.Split(line[1:len(line)-1], "|")
	for i := range raw {
		raw[i] = strings.TrimSpace(raw[i])
	}
	return raw, true
}

func splitDataTableRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
		return nil, false
	}
	raw := strings.Split(line[1:len(line)-1], "|")
	for i := range raw {
		if !strings.HasPrefix(raw[i], " ") || !strings.HasSuffix(raw[i], " ") {
			return nil, false
		}
		raw[i] = strings.TrimSuffix(strings.TrimPrefix(raw[i], " "), " ")
	}
	return raw, true
}

func validTableSeparator(line string, columns int) bool {
	cells, ok := splitTableRow(line)
	if !ok || len(cells) != columns {
		return false
	}
	for _, cell := range cells {
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func entryRow(entry Entry) (string, error) {
	if err := validateEntry(entry); err != nil {
		return "", err
	}
	return fmt.Sprintf("| %s | %s | %s |\n", entry.Hash, encodeCell(entry.Server), encodeCell(entry.Path)), nil
}

func shardPath(manifestPath, slot, prefix string, part int) string {
	return fmt.Sprintf("%s/%s/%s-%03d.md", shardRoot(manifestPath), slot, prefix, part)
}

func shardRoot(manifestPath string) string {
	if base, ok := strings.CutSuffix(manifestPath, ".md"); ok {
		return base + ".shards"
	}
	return manifestPath + ".shards"
}

func parseShardPath(manifestPath, slot, shard string) (prefix string, part int, err error) {
	root := shardRoot(manifestPath) + "/" + slot
	if path.Dir(shard) != root {
		return "", 0, errors.New("shard path is outside active slot")
	}
	return parseShardFile(path.Base(shard))
}

func parseShardFile(name string) (prefix string, part int, err error) {
	if !strings.HasSuffix(name, ".md") {
		return "", 0, errors.New("shard filename must end in .md")
	}
	prefix, partText, ok := strings.Cut(strings.TrimSuffix(name, ".md"), "-")
	if !ok || !validPrefix(prefix) {
		return "", 0, errors.New("invalid shard filename")
	}
	part, err = parseNonNegative("shard part", partText)
	if err != nil || name != fmt.Sprintf("%s-%03d.md", prefix, part) {
		return "", 0, errors.New("invalid shard part filename")
	}
	return prefix, part, nil
}

func matchingEntries(entries []Entry, hash string) []Entry {
	var matches []Entry
	for _, entry := range entries {
		if entry.Hash == hash {
			matches = append(matches, entry)
		}
	}
	return matches
}

func compareEntries(a, b Entry) int {
	if c := strings.Compare(a.Hash, b.Hash); c != 0 {
		return c
	}
	if c := strings.Compare(a.Server, b.Server); c != 0 {
		return c
	}
	return strings.Compare(a.Path, b.Path)
}

func compareShardRefs(a, b ShardRef) int {
	if c := strings.Compare(a.Prefix, b.Prefix); c != 0 {
		return c
	}
	_, aPart, aErr := parseShardFile(path.Base(a.Path))
	_, bPart, bErr := parseShardFile(path.Base(b.Path))
	if aErr == nil && bErr == nil {
		if aPart < bPart {
			return -1
		}
		if aPart > bPart {
			return 1
		}
	}
	return strings.Compare(a.Path, b.Path)
}

func validPrefix(prefix string) bool {
	if len(prefix) != HashPrefixLength {
		return false
	}
	for _, c := range prefix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validateManifestPath(manifestPath string) error {
	if err := protocol.ValidateRequestPath(manifestPath); err != nil || path.Clean(manifestPath) != manifestPath || manifestPath == "/" {
		return fmt.Errorf("invalid manifest path %q", manifestPath)
	}
	generated := shardPath(manifestPath, SlotA, "00", 0)
	if err := protocol.ValidateRequestPath(generated); err != nil {
		return fmt.Errorf("manifest path cannot produce valid shard paths: %w", err)
	}
	if err := protocol.ValidateRequestPath(VersionPath(generated, math.MaxInt)); err != nil {
		return fmt.Errorf("manifest path cannot produce versioned shard paths: %w", err)
	}
	return nil
}

func validateEntry(entry Entry) error {
	cleanHash, ok := protocol.IsHashPath(entry.Hash)
	if !ok || cleanHash != entry.Hash {
		return fmt.Errorf("invalid content hash %q", entry.Hash)
	}
	server, err := url.Parse(entry.Server)
	if err != nil || server.Scheme != "mark" || server.Hostname() == "" ||
		server.User != nil || (server.Path != "" && server.Path != "/") || server.RawQuery != "" || server.Fragment != "" ||
		strings.ContainsAny(entry.Server, "\r\n\t") {
		return fmt.Errorf("invalid index server %q", entry.Server)
	}
	if port := server.Port(); port != "" {
		n, portErr := strconv.Atoi(port)
		if portErr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid index server %q", entry.Server)
		}
	}
	if err := protocol.ValidateRequestPath(entry.Path); err != nil || entry.Path == "/" ||
		path.Clean(entry.Path) != entry.Path {
		return fmt.Errorf("invalid index path %q", entry.Path)
	}
	return nil
}

func documentFormat(body string) (string, error) {
	heading := firstNonEmptyLine(body)
	if heading != "# Content Index Manifest" && heading != "# Content Index Shard" {
		return "", nil
	}
	meta, err := generatedMetadata(body, heading)
	if err != nil {
		return "", err
	}
	return meta["Format"], nil
}

func generatedMetadata(body, wantHeading string) (map[string]string, error) {
	lines := strings.Split(body, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != wantHeading {
		return nil, fmt.Errorf("generated document heading must be %q", wantHeading)
	}
	meta := make(map[string]string)
	for _, rawLine := range lines[1:] {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "|") {
			break
		}
		if line == "" {
			continue
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "> ") {
			return nil, fmt.Errorf("unexpected generated document preamble line %q", line)
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "> "), ":")
		if !ok {
			return nil, fmt.Errorf("invalid metadata line %q", line)
		}
		key = strings.TrimSpace(key)
		if _, duplicate := meta[key]; duplicate {
			return nil, fmt.Errorf("duplicate metadata field %q", key)
		}
		meta[key] = strings.TrimSpace(value)
	}
	return meta, nil
}

func firstNonEmptyLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func parsePositive(name, raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid %s value %q", name, raw)
	}
	return n, nil
}

func parseNonNegative(name, raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s value %q", name, raw)
	}
	return n, nil
}

func encodeCell(value string) string {
	value = strings.ReplaceAll(value, "%", "%25")
	value = strings.ReplaceAll(value, "|", "%7C")
	return strings.ReplaceAll(value, " ", "%20")
}

func decodeCell(value string) string {
	value = strings.ReplaceAll(value, "%20", " ")
	value = strings.ReplaceAll(value, "%7C", "|")
	return strings.ReplaceAll(value, "%25", "%")
}
