// Package protocol implements the Mark Protocol specification for Demarkus.
package protocol

import (
	"strings"
	"unicode/utf8"
)

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

	// WellKnownManifestPath is the conventional path for agent manifest discovery.
	WellKnownManifestPath = "/.well-known/agent-manifest.md"

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
