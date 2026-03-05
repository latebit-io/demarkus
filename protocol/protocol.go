// Package protocol implements the Mark Protocol specification for Demarkus.
package protocol

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
)
