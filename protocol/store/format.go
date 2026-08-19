package store

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// This file exports the version-file format as shared vocabulary. A stored
// version is a store-managed frontmatter block (version, archived,
// previous-hash, publisher metadata) followed by the document content. Every
// DocumentStore backend persists these exact bytes so the hash chain,
// metadata extraction, and migration between backends stay byte-identical.

// SerializeVersion constructs the stored bytes for a version: store
// frontmatter followed by content. prevRaw is the previous version's stored
// bytes, hashed into the previous-hash chain link; it is required for
// version > 1 and ignored for version 1.
func SerializeVersion(version int, prevRaw, content []byte, meta map[string]string) ([]byte, error) {
	if err := validateMeta(meta); err != nil {
		return nil, err
	}
	if version > 1 && len(prevRaw) == 0 {
		return nil, fmt.Errorf("serialize v%d: previous version bytes required for hash chain", version)
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("version: %d\n", version))
	sb.WriteString("archived: false\n")
	if version > 1 {
		h := sha256.Sum256(prevRaw)
		sb.WriteString(fmt.Sprintf("previous-hash: sha256-%x\n", h))
	}
	if len(meta) > 0 {
		keys := make([]string, 0, len(meta))
		for k := range meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch {
			case k == "tags" && okfKeys[k]:
				sb.WriteString(fmt.Sprintf("tags: %s\n", formatTagsList(meta[k])))
			case okfKeys[k]:
				sb.WriteString(fmt.Sprintf("%s: %s\n", k, meta[k]))
			default:
				sb.WriteString(fmt.Sprintf("%s%s: %s\n", metaPrefix, k, meta[k]))
			}
		}
	}
	sb.WriteString("---\n")
	return append([]byte(sb.String()), content...), nil
}

// SetArchived rewrites the archived flag in a stored version's frontmatter,
// returning the new bytes. The input must be a well-formed version file.
// Only the flag changes; content and every other frontmatter field are
// preserved, so this is the in-place archive toggle both backends share.
func SetArchived(data []byte, archived bool) ([]byte, error) {
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("invalid version file format")
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return nil, fmt.Errorf("invalid version file format")
	}
	frontmatter := content[4 : 4+end]
	rest := content[4+end+5:]

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
	return []byte("---\n" + strings.Join(lines, "\n") + "\n---\n" + rest), nil
}

// ExtractBody returns the document content with the store frontmatter block
// stripped. Data without a frontmatter block is returned unchanged.
func ExtractBody(data []byte) []byte { return extractBody(data) }

// ExtractMetadata parses publisher metadata from store frontmatter, returning
// a bare-keyed map (or nil if none found).
func ExtractMetadata(data []byte) map[string]string { return extractMetadata(data) }

// IsArchived reports whether stored version bytes are marked archived in
// their frontmatter.
func IsArchived(data []byte) bool { return isArchived(data) }

// ExtractPreviousHash returns the previous-hash chain link recorded in a
// stored version's frontmatter, or "" when absent.
func ExtractPreviousHash(data []byte) string { return extractPreviousHash(data) }

// ContentHash returns the canonical content-addressing hash ("sha256-<hex>")
// of a document body. LookupHash is keyed by this value.
func ContentHash(body []byte) string { return contentHash(body) }

// MetaEqual reports whether two publisher metadata maps are equal. Both nil
// and empty compare equal.
func MetaEqual(a, b map[string]string) bool { return metaEqual(a, b) }

// JoinContent concatenates existing and new content with a newline separator
// when needed, enforcing protocol.MaxBodyLength via ErrSizeLimit.
func JoinContent(existing, content []byte) ([]byte, error) { return joinContent(existing, content) }

// RetentionValue returns the retention count declared in publisher metadata,
// or 0 when absent or invalid.
func RetentionValue(meta map[string]string) int { return retentionValue(meta) }

// PrepareAppendMeta builds the publisher metadata an APPEND stores: the base
// version's merged with the request's, then typed (SPEC 6.6). Both backends
// call it so the rules cannot drift; the write itself validates the result.
func PrepareAppendMeta(reqPath string, base, req map[string]string) map[string]string {
	return ApplyOKFTypeDefault(reqPath, mergeAppendMeta(base, req))
}

// MaxStoreFrontmatter is the maximum overhead the store-managed frontmatter
// adds to stored version bytes; backends bound stored size by
// protocol.MaxBodyLength + MaxStoreFrontmatter.
const MaxStoreFrontmatter = maxStoreFrontmatter

// ValidateWrite checks the size, metadata, and encoding of a write request
// in the order every backend applies before reading any state.
func ValidateWrite(content []byte, meta map[string]string) error { return validateWrite(content, meta) }
