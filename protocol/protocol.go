// Package protocol implements the Mark Protocol specification for Demarkus.
package protocol

import "strings"

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

	// WellKnownManifestPath is the conventional path for agent manifest discovery.
	WellKnownManifestPath = "/.well-known/agent-manifest.md"

	// MaxMetaKeys is the maximum number of publisher metadata keys.
	MaxMetaKeys = 10

	// MaxMetaBytes is the approximate maximum size of publisher metadata
	// (sum of key and value lengths, excluding serialization overhead).
	MaxMetaBytes = 512
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
// serialization: no carriage returns or newlines.
func IsValidMetaValue(v string) bool {
	return !strings.ContainsAny(v, "\r\n")
}
