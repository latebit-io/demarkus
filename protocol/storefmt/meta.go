package storefmt

import (
	"fmt"
	"maps"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol"
)

// MaxStoreFrontmatter is the maximum overhead the store-managed frontmatter
// adds to a version file (version, archived, previous-hash, publisher
// metadata, and delimiters). It must cover MaxMetaBytes of publisher metadata
// plus the operational fields and per-line serialization overhead.
const MaxStoreFrontmatter = 2048

// metaPrefix is the key prefix for non-spec publisher metadata in store
// frontmatter. On disk: "meta.importance: 0.8". Stripped when returned to
// clients. Keys recognized by the Open Knowledge Format (see okfKeys) are
// written bare instead, so the store frontmatter matches the OKF spec for the
// fields it defines.
const metaPrefix = "meta."

// okfKeys are the publisher metadata keys recognized by the Open Knowledge
// Format (OKF) spec. They serialize as bare frontmatter fields (e.g. "tags:")
// to conform to the spec; every other publisher key keeps the metaPrefix. The
// in-memory metadata map is bare-keyed either way — only on-disk serialization
// differs.
var okfKeys = map[string]bool{
	"type":        true,
	"title":       true,
	"description": true,
	"resource":    true,
	"tags":        true,
	"timestamp":   true,
}

// retentionKey is the publisher metadata key that bounds a document's version
// history. When the just-written version carries it, the write prunes the
// oldest versions so at most that many remain (current included). It is
// publisher metadata, not a reserved store field: any writer with publish
// capability may set it, the same trust level that can archive the document.
// Absent retention means keep every version — the default is unchanged.
const retentionKey = "retention"

// reservedMetaKeys are bare frontmatter fields owned by the store. Publishers
// may not set them (validateMeta rejects them) and extractMetadata never
// surfaces them as publisher metadata, preventing a publisher from forging
// store state such as version or archival.
var reservedMetaKeys = map[string]bool{
	"version":       true,
	"previous-hash": true,
	"archived":      true,
}

// IsReservedMetaKey reports whether a publisher metadata key is reserved by the
// store and would be rejected on publish. Callers that build metadata from
// untrusted sources (e.g. importing external documents) use this to drop or
// rename colliding keys before publishing.
func IsReservedMetaKey(key string) bool {
	return reservedMetaKeys[key]
}

// ParseRetention parses a retention metadata value. ok is true only for a
// positive integer — the only form that prunes; validateMeta rejects every
// other value. Exported so clients gating destructive-confirmation UX (the
// CLI prompt) share the exact predicate the server enforces instead of
// re-implementing it and drifting.
func ParseRetention(v string) (n int, ok bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// RetentionValue returns the retention count declared in publisher metadata,
// or 0 when absent. validateMeta has already rejected non-integer or < 1
// values, so a parse failure here means the map bypassed validation — treat
// it as no retention rather than pruning on a value that was never vetted.
func RetentionValue(meta map[string]string) int {
	v, ok := meta[retentionKey]
	if !ok {
		return 0
	}
	n, ok := ParseRetention(v)
	if !ok {
		return 0
	}
	return n
}

// mergeAppendMeta layers an APPEND's metadata over the base version's, request
// keys winning (SPEC 6.6); without it an append drops the doc out of LOOKUP.
// retention is never inherited: pruning belongs to the write declaring it.
func mergeAppendMeta(base, req map[string]string) map[string]string {
	if len(base) == 0 && len(req) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(req))
	maps.Copy(merged, base)
	delete(merged, retentionKey)
	maps.Copy(merged, req)
	return merged
}

// ApplyOKFTypeDefault types every written concept document (SPEC 6.4), leaving
// a declared type and the reserved OKF files alone. It always returns a copy:
// the caller's map may belong to a fetched Document. nil stays nil.
func ApplyOKFTypeDefault(reqPath string, meta map[string]string) map[string]string {
	switch path.Base(reqPath) {
	case "index.md", "log.md":
		return maps.Clone(meta)
	}
	if strings.TrimSpace(meta["type"]) != "" {
		return maps.Clone(meta)
	}
	typed := make(map[string]string, len(meta)+1)
	maps.Copy(typed, meta)
	typed["type"] = protocol.OKFDefaultType
	return typed
}

// ValidateDocumentContent enforces the document contract for PUBLISH/APPEND: a
// .md path with a UTF-8 body. Callers map the errors to bad-request.
func ValidateDocumentContent(reqPath string, content []byte) error {
	if !strings.HasSuffix(reqPath, ".md") {
		return ErrInvalidPath
	}
	return ValidateBody(content)
}

// ValidateBody checks a body is UTF-8 text. Each backend's Write calls it as
// defense in depth. No .md check here: the store is deliberately path-agnostic.
func ValidateBody(content []byte) error {
	if !utf8.Valid(content) {
		return ErrInvalidContent
	}
	return nil
}

// ValidateWrite checks the size, metadata, and encoding of a write request.
func ValidateWrite(content []byte, meta map[string]string) error {
	if int64(len(content)) > protocol.MaxBodyLength {
		return ErrSizeLimit
	}
	if err := ValidateMeta(meta); err != nil {
		return err
	}
	return ValidateBody(content)
}

// ValidateMeta checks metadata is safe for frontmatter serialization. Defense
// in depth: the handler validates too, but the store is callable off the
// network path. Failures wrap ErrInvalidMeta so callers can classify them.
func ValidateMeta(meta map[string]string) error {
	size := 0
	for k, v := range meta {
		if reservedMetaKeys[k] {
			return fmt.Errorf("%w: key %q is reserved by the store", ErrInvalidMeta, k)
		}
		if !protocol.IsValidMetaKey(k) {
			return fmt.Errorf("%w: key %q contains invalid characters", ErrInvalidMeta, k)
		}
		if !protocol.IsValidMetaValue(v) {
			return fmt.Errorf("%w: value for key %q is not valid single-line UTF-8", ErrInvalidMeta, k)
		}
		if k == retentionKey {
			if _, ok := ParseRetention(v); !ok {
				return fmt.Errorf("%w: key %q must be a positive integer, got %q", ErrInvalidMeta, retentionKey, v)
			}
		}
		size += SerializedMetaSize(k, v)
	}
	if len(meta) > protocol.MaxMetaKeys {
		return fmt.Errorf("%w: too many metadata keys (max %d)", ErrInvalidMeta, protocol.MaxMetaKeys)
	}
	if size > protocol.MaxMetaBytes {
		return fmt.Errorf("%w: metadata too large (max %d bytes)", ErrInvalidMeta, protocol.MaxMetaBytes)
	}
	return nil
}

// ExtractMetadata parses publisher metadata from store frontmatter, returning a
// bare-keyed map (or nil if none found). Two on-disk forms are recognized:
// recognized OKF fields written bare (okfKeys, "tags" parsed from its YAML
// list), and every other publisher key carried under metaPrefix (stripped
// here). Reserved store fields (reservedMetaKeys) and any other bare key are
// ignored, so operational state never surfaces as publisher metadata. An older
// "meta."-prefixed tags value is read as-is, keeping prior writes readable.
func ExtractMetadata(data []byte) map[string]string {
	split, ok := splitStored(data)
	if !ok {
		return nil
	}
	block := string(split.Block)
	var meta map[string]string
	set := func(k, v string) {
		if meta == nil {
			meta = make(map[string]string)
		}
		meta[k] = v
	}
	for line := range strings.SplitSeq(block, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimRight(val, "\r")
		switch {
		case reservedMetaKeys[key]:
			// Operational field — never publisher metadata.
		case strings.HasPrefix(key, metaPrefix):
			// Re-check the unprefixed key so a stored "meta.archived" cannot
			// resurrect a reserved operational field as publisher metadata.
			if k := key[len(metaPrefix):]; !reservedMetaKeys[k] {
				set(k, val)
			}
		case key == "tags":
			set(key, parseTagsList(val))
		case okfKeys[key]:
			set(key, val)
		}
	}
	return meta
}

// SerializedMetaSize returns the byte size a publisher key/value pair counts
// against MaxMetaBytes: the key length plus the value length as actually
// serialized on disk. The OKF "tags" field is stored as a YAML flow list, which
// is longer than its comma-separated map form, so counting the raw value would
// undercount the on-disk size and let a tag-heavy document slip past the budget
// only to overflow the frontmatter. Per-line delimiters and the "meta." prefix
// are fixed, bounded overhead covered by maxStoreFrontmatter, not counted here.
func SerializedMetaSize(key, value string) int {
	if key == "tags" {
		return len(key) + len(FormatTagsList(value))
	}
	return len(key) + len(value)
}

// FormatTagsList serializes a comma-separated tag string as an OKF YAML flow
// list, e.g. "sales,revenue" → "[sales, revenue]". Empty input yields "[]".
func FormatTagsList(csv string) string {
	return "[" + strings.Join(protocol.SplitTags(csv), ", ") + "]"
}

// parseTagsList parses a tags value back to the comma-separated form held in the
// metadata map. It accepts both the OKF YAML flow list ("[sales, revenue]") and
// a bare comma-separated string (older "meta.tags" writes), so prior versions
// stay readable.
func parseTagsList(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = v[1 : len(v)-1]
	}
	return strings.Join(protocol.SplitTags(v), ",")
}

// MetaEqual reports whether two metadata maps are equal.
// Treats nil and empty maps as equal.
func MetaEqual(a, b map[string]string) bool { return maps.Equal(a, b) }
