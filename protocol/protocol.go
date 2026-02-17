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
)
