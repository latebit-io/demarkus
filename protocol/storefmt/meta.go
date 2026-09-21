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

// MaxStoreFrontmatter bounds the frontmatter a version file carries: the
// operational fields, MaxMetaBytes of publisher metadata and line overhead.
const MaxStoreFrontmatter = 2048

// metaPrefix marks publisher keys the Open Knowledge Format does not define,
// on disk "meta.importance: 0.8"; it is stripped before clients see the key.
const metaPrefix = "meta."

// okfKeys are the publisher keys the OKF spec defines; they serialize bare
// ("tags:") while every other key takes metaPrefix. In memory all keys are bare.
var okfKeys = map[string]bool{
	"type":        true,
	"title":       true,
	"description": true,
	"resource":    true,
	"tags":        true,
	"timestamp":   true,
}

// retentionKey bounds a document's history: a write carrying it prunes down to
// that many versions. Publisher metadata, so any writer who may archive may set
// it; absent means keep every version.
const retentionKey = "retention"

// reservedMetaKeys are store owned fields. ValidateMeta refuses them and
// ExtractMetadata never returns them, so a publisher cannot forge store state.
var reservedMetaKeys = map[string]bool{
	"version":       true,
	"previous-hash": true,
	"archived":      true,
}

// IsReservedMetaKey reports a key a publish would refuse, so importers of
// outside documents can drop or rename it first.
func IsReservedMetaKey(key string) bool {
	return reservedMetaKeys[key]
}

// ParseRetention accepts only a positive integer, the one form that prunes.
// Clients that confirm destructive writes share this predicate with the server.
func ParseRetention(v string) (n int, ok bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// RetentionValue is the declared retention, or 0. A value that fails to parse
// bypassed ValidateMeta, so it counts as no retention rather than pruning.
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

// ExtractMetadata returns bare keyed publisher metadata, or nil: OKF fields
// read bare, other keys under metaPrefix, reserved and unknown bare keys
// skipped. An older "meta.tags" value still reads.
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

// SerializedMetaSize is what a pair costs against MaxMetaBytes: key plus value
// as written. Tags count as their longer YAML list form, or a tag heavy
// document would pass the budget and overflow the frontmatter.
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

// parseTagsList returns tags in the comma separated map form, from the OKF
// flow list or from an older bare "meta.tags" string.
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
