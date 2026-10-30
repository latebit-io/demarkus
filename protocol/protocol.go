// Package protocol implements the Mark Protocol specification for Demarkus.
package protocol

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxListPageSize is the maximum number of entries in one LIST response.
const MaxListPageSize = 1000

// SplitTags parses a comma-separated tag list into trimmed, non-empty tags.
func SplitTags(s string) []string {
	var tags []string
	for raw := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(raw); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// MergeTags adds the trimmed, non-empty tags of add that have is missing;
// have's own empty entries are dropped.
func MergeTags(have, add []string) []string {
	out := slices.DeleteFunc(slices.Clone(have), func(t string) bool { return t == "" })
	for _, t := range add {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// IsWriteSuccess reports a PUBLISH or APPEND status that landed.
func IsWriteSuccess(status string) bool {
	return status == StatusOK || status == StatusCreated
}

// reservedMetadataKeys are server-owned response metadata keys; publishers
// cannot set them and clients never treat them as publisher metadata.
var reservedMetadataKeys = map[string]bool{
	"version":         true,
	"modified":        true,
	"etag":            true,
	"content-hash":    true,
	"current-version": true,
	"server-version":  true,
	"your-version":    true,
	"total":           true,
	"current":         true,
	"chain-valid":     true,
	"chain-error":     true,
	"archived":        true,
	"entries":         true,
	"matches":         true,
	"match":           true,
	"status":          true,
	"warning":         true, // what a landed write's gate let through
}

// IsReservedMetadataKey reports a server-owned response metadata key.
func IsReservedMetadataKey(key string) bool {
	return reservedMetadataKeys[key]
}

// controlMetadataKeys are request metadata keys a server consumes as request
// controls and never stores.
var controlMetadataKeys = map[string]bool{
	"auth":              true,
	"expected-version":  true,
	"if-none-match":     true,
	"if-modified-since": true,
}

// IsControlMetadataKey reports a request control key, never publisher metadata.
func IsControlMetadataKey(key string) bool {
	return controlMetadataKeys[key]
}

// Identity keys are ordinary publisher metadata a writing surface stamps and
// never lets a caller set; servers store them opaquely and echo them on
// WATCH events.
const (
	MetaAgent = "agent" // the writing software
	MetaUser  = "user"  // the verified person behind the write
)

// IsIdentityMetadataKey reports a surface-stamped identity key.
func IsIdentityMetadataKey(key string) bool {
	return key == MetaAgent || key == MetaUser
}

const (
	// DefaultPort is the default port for Mark Protocol servers.
	DefaultPort = 6309

	// ALPN is the application-layer protocol negotiation identifier.
	ALPN = "mark"

	// VerbFetch retrieves a document.
	VerbFetch = "FETCH"

	// VerbList retrieves directory contents.
	VerbList = "LIST"

	// VerbVersions retrieves the version history of a document.
	VerbVersions = "VERSIONS"

	// VerbPublish creates or updates a document, creating a new immutable version.
	VerbPublish = "PUBLISH"

	// VerbArchive marks a document as archived, preventing it from being served.
	VerbArchive = "ARCHIVE"

	// VerbAppend appends content to the end of an existing document.
	VerbAppend = "APPEND"

	// VerbLookup looks up documents by subject against the server's catalog,
	// returning an importance-ranked list of matches.
	VerbLookup = "LOOKUP"

	// VerbWatch subscribes to change hints under a path prefix (§6.8). The
	// response is a stream of blocks, see watch.go.
	VerbWatch = "WATCH"

	// WellKnownManifestPath is the conventional path for agent manifest discovery.
	WellKnownManifestPath = "/.well-known/agent-manifest.md"

	// MatchCatalog is the LOOKUP match mode over tags and title (§6.7).
	MatchCatalog = "catalog"
	// MatchBody is the LOOKUP match mode over the section index of bodies.
	MatchBody = "body"
	// MatchSimilar answers which documents the request body repeats (§6.7).
	MatchSimilar = "similar"

	// MaxMetaKeys is the maximum number of publisher metadata keys. Sized to
	// hold the recognized OKF fields (type, title, description, resource, tags,
	// timestamp), demarkus's importance, and generous headroom for
	// producer-defined keys. MaxMetaBytes still bounds the actual on-disk size.
	MaxMetaKeys = 50

	// MaxMetaBytes is the approximate maximum size of publisher metadata: the
	// sum of key lengths and on-disk-serialized value lengths, excluding
	// per-line delimiters and the "meta." prefix. Values are counted as stored
	// (notably "tags" as its YAML list form, see store.SerializedMetaSize), so
	// the cap is an honest bound on frontmatter size. This total, not the key
	// count, is the binding limit for a typical document.
	MaxMetaBytes = 1024

	// OKFDefaultType is the Open Knowledge Format `type` the server assigns to a
	// published document that declares none, so every stored concept document is
	// a typed OKF concept by construction. Reserved OKF files (index.md, log.md)
	// are exempt — OKF defines them as navigation/history, not concepts.
	OKFDefaultType = "Document"
)

// IsValidMetaKey checks that a metadata key contains only safe characters
// for frontmatter serialization: lowercase letters, digits, and hyphens.
func IsValidMetaKey(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range k {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// IsValidMetaValue checks that a metadata value is safe for frontmatter
// serialization: valid UTF-8 without carriage returns or newlines.
func IsValidMetaValue(v string) bool {
	return utf8.ValidString(v) && !strings.ContainsAny(v, "\r\n")
}

// worldNameRE is a DNS label. A world name is the host of every mark:// URL
// into the world, a graph key and part of a Secret name, so nothing looser
// works everywhere; a bad name is rejected, never normalized.
var worldNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// IsWorldName reports whether name is a valid world name.
func IsWorldName(name string) bool { return worldNameRE.MatchString(name) }

// ParseMatch validates a LOOKUP match value; empty means catalog. It is the
// one rule server and clients apply, so a typo is rejected the same way
// before and after the wire.
func ParseMatch(match string) (string, error) {
	switch match {
	case "", MatchCatalog:
		return MatchCatalog, nil
	case MatchBody:
		return MatchBody, nil
	case MatchSimilar:
		return MatchSimilar, nil
	}
	return "", fmt.Errorf("match must be %q, %q or %q, got %q", MatchCatalog, MatchBody, MatchSimilar, match)
}

// MatchSectioned reports a mode answered from the section index, whose rows
// are sections with a snippet.
func MatchSectioned(match string) bool { return match == MatchBody || match == MatchSimilar }

// Downgraded reports a lookup answered in a lesser mode than it asked for:
// the server had no section index for the snapshot and said so.
func Downgraded(requested, answered string) bool {
	return requested != "" && requested != MatchCatalog && answered != requested
}
