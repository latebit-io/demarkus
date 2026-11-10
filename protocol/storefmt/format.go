package storefmt

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// MaxVersionNumber fits int32 so any backend's integer column and every Go architecture agree.
const MaxVersionNumber = 1<<31 - 1

// StoredVersionHeader is the validated operational part of stored frontmatter.
type StoredVersionHeader struct {
	Version      int
	Archived     bool
	PreviousHash string
	Path         string // canonical document path; "" in versions written before the store recorded it
}

// CheckNames reports a header that names another document than docPath or,
// when version > 0, another version. A legacy header names no path.
func (h StoredVersionHeader) CheckNames(docPath string, version int) error {
	if version > 0 && h.Version != version {
		return fmt.Errorf("stored version is %d, not %d", h.Version, version)
	}
	if h.Path != "" && h.Path != docPath {
		return fmt.Errorf("stored path is %q, not %q", h.Path, docPath)
	}
	return nil
}

// Stored version: store frontmatter (version, archived, previous-hash, path,
// publisher metadata) then content. Every backend persists these exact bytes
// so hash chains, metadata extraction and exports stay byte-identical.

// VersionSpec is one version to serialize. Path is the document's canonical
// path. Previous is the prior version's stored bytes, hashed into the
// previous-hash chain link: required past version 1, ignored for version 1.
type VersionSpec struct {
	Path     string
	Version  int
	Previous []byte
	Content  []byte
	Metadata map[string]string
}

// SerializeVersion constructs the stored bytes for a version: store
// frontmatter followed by content.
func SerializeVersion(spec *VersionSpec) ([]byte, error) {
	if spec.Version < 1 || spec.Version > MaxVersionNumber {
		return nil, fmt.Errorf("serialize version: %d is outside [1,%d]", spec.Version, MaxVersionNumber)
	}
	if !ValidDocumentPath(spec.Path) {
		return nil, fmt.Errorf("serialize version: %q is not a canonical document path", spec.Path)
	}
	if err := ValidateMeta(spec.Metadata); err != nil {
		return nil, err
	}
	if spec.Version > 1 && len(spec.Previous) == 0 {
		return nil, fmt.Errorf("serialize v%d: previous version bytes required for hash chain", spec.Version)
	}
	var sb strings.Builder
	sb.WriteString(protocol.FrontmatterFence)
	fmt.Fprintf(&sb, "version: %d\n", spec.Version)
	sb.WriteString("archived: false\n")
	if spec.Version > 1 {
		h := sha256.Sum256(spec.Previous)
		fmt.Fprintf(&sb, "previous-hash: sha256-%x\n", h)
	}
	fmt.Fprintf(&sb, "%s: %s\n", storePathKey, spec.Path)
	if len(spec.Metadata) > 0 {
		keys := make([]string, 0, len(spec.Metadata))
		for k := range spec.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch {
			case k == "tags" && okfKeys[k]:
				fmt.Fprintf(&sb, "tags: %s\n", FormatTagsList(spec.Metadata[k]))
			case okfKeys[k]:
				fmt.Fprintf(&sb, "%s: %s\n", k, spec.Metadata[k])
			default:
				fmt.Fprintf(&sb, "%s%s: %s\n", metaPrefix, k, spec.Metadata[k])
			}
		}
	}
	sb.WriteString(protocol.FrontmatterFence)
	return append([]byte(sb.String()), spec.Content...), nil
}

// SetArchived rewrites the archived flag in stored bytes, for test fixtures
// only: production archive transitions use the sidecar so stored versions
// stay immutable.
func SetArchived(data []byte, archived bool) ([]byte, error) {
	split, ok := splitStored(data)
	if !ok {
		return nil, fmt.Errorf("invalid version file format")
	}
	frontmatter := string(split.Block)
	rest := string(split.Body)

	flag := "archived: false"
	if archived {
		flag = "archived: true"
	}
	lines := strings.Split(frontmatter, "\n")
	found := false
	for i, line := range lines {
		key, _, ok := strings.Cut(line, ": ")
		if ok && strings.TrimSpace(key) == "archived" {
			lines[i] = flag
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, flag)
	}
	return []byte(protocol.FrontmatterFence + strings.Join(lines, "\n") + "\n" + protocol.FrontmatterFence + rest), nil
}

// NormalizeMetadata returns metadata as persisted by SerializeVersion.
func NormalizeMetadata(meta map[string]string) map[string]string {
	normalized := maps.Clone(meta)
	if tags, exists := normalized["tags"]; exists {
		normalized["tags"] = parseTagsList(FormatTagsList(tags))
	}
	return normalized
}

// StoredETag returns the protocol ETag for exact stored-version bytes.
func StoredETag(stored []byte) string {
	hash := sha256.Sum256(stored)
	return fmt.Sprintf("%x", hash)
}

// InspectStoredVersion validates canonical operational and publisher fields.
func InspectStoredVersion(stored []byte) (StoredVersionHeader, error) {
	split, ok := splitStored(stored)
	if !ok {
		return StoredVersionHeader{}, fmt.Errorf("stored version: invalid frontmatter")
	}
	header, publisher, err := inspectOperationalFields(strings.Split(string(split.Block), "\n"))
	if err != nil {
		return header, err
	}
	if err := inspectPublisherFields(publisher); err != nil {
		return header, err
	}
	if err := ValidateMeta(ExtractMetadata(stored)); err != nil {
		return header, fmt.Errorf("stored version: %w", err)
	}
	return header, nil
}

// inspectOperationalFields parses the positional store fields and returns
// the publisher lines after them.
func inspectOperationalFields(lines []string) (StoredVersionHeader, []string, error) {
	var header StoredVersionHeader
	if len(lines) < 2 {
		return header, nil, fmt.Errorf("stored version: missing operational fields")
	}
	versionValue, ok := exactStoredField(lines[0], "version")
	if !ok {
		return header, nil, fmt.Errorf("stored version: malformed version field %q", lines[0])
	}
	version, err := strconv.ParseInt(versionValue, 10, 32)
	if err != nil || version < 1 || strconv.FormatInt(version, 10) != versionValue {
		return header, nil, fmt.Errorf("stored version: invalid version %q", versionValue)
	}
	header.Version = int(version)

	archivedValue, ok := exactStoredField(lines[1], "archived")
	if !ok || (archivedValue != "false" && archivedValue != "true") {
		return header, nil, fmt.Errorf("stored version: invalid archived field %q", lines[1])
	}
	header.Archived = archivedValue == "true"

	rest := lines[2:]
	if header.Version > 1 {
		if len(rest) == 0 {
			return header, nil, fmt.Errorf("stored version: missing previous-hash")
		}
		previousHash, ok := exactStoredField(rest[0], "previous-hash")
		if !ok || !validStoredHash(previousHash) {
			return header, nil, fmt.Errorf("stored version: invalid previous-hash %q", rest[0])
		}
		header.PreviousHash = previousHash
		rest = rest[1:]
	}
	if len(rest) > 0 {
		if docPath, ok := exactStoredField(rest[0], storePathKey); ok {
			if !ValidDocumentPath(docPath) {
				return header, nil, fmt.Errorf("stored version: invalid path %q", docPath)
			}
			header.Path = docPath
			rest = rest[1:]
		}
	}
	return header, rest, nil
}

// inspectPublisherFields checks publisher lines are OKF or meta. keys,
// strictly sorted by logical key.
func inspectPublisherFields(lines []string) error {
	previousKey := ""
	for _, line := range lines {
		key, _, ok := strings.Cut(line, ": ")
		if !ok || key == "" || strings.TrimSpace(key) != key {
			return fmt.Errorf("stored version: malformed publisher field %q", line)
		}
		logicalKey := key
		switch {
		case key == storePathKey:
			return fmt.Errorf("stored version: store field %q out of place", key)
		case okfKeys[key]:
		case strings.HasPrefix(key, metaPrefix):
			logicalKey = strings.TrimPrefix(key, metaPrefix)
			if reservedMetaKeys[logicalKey] {
				return fmt.Errorf("stored version: field %q aliases canonical field %q", key, logicalKey)
			}
		default:
			return fmt.Errorf("stored version: unknown field %q", key)
		}
		if logicalKey <= previousKey {
			return fmt.Errorf("stored version: publisher fields are not strictly sorted")
		}
		previousKey = logicalKey
	}
	return nil
}

// StoredVersionNumber returns the exact version declared by stored frontmatter.
func StoredVersionNumber(stored []byte) (int, error) {
	header, err := InspectStoredVersion(stored)
	if err != nil {
		return 0, err
	}
	return header.Version, nil
}

func exactStoredField(line, key string) (string, bool) {
	prefix := key + ": "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	return strings.TrimPrefix(line, prefix), true
}

func validStoredHash(value string) bool { return protocol.IsContentHash(value) }

// PublishedMeta is the publisher metadata a PUBLISH of req stores at reqPath:
// the map replaced whole, typed, tags in stored form. Reconciling clients and
// test doubles share it so their idea of a landed write cannot drift.
func PublishedMeta(reqPath string, req map[string]string) map[string]string {
	return NormalizeMetadata(ApplyOKFTypeDefault(reqPath, req))
}

// PrepareAppendMeta builds the publisher metadata an APPEND stores: the base
// version's merged with the request's, then typed (SPEC 6.6). Both backends
// call it so the rules cannot drift; the write itself validates the result.
func PrepareAppendMeta(reqPath string, base, req map[string]string) map[string]string {
	return ApplyOKFTypeDefault(reqPath, mergeAppendMeta(base, req))
}
