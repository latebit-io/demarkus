package storefmt

import (
	"errors"
	"fmt"
	"time"
)

// Document holds a document's content and metadata. Content is always the
// body only — store frontmatter is stripped on every path (Get, Write,
// WriteVersion, Append); operational state travels in the dedicated fields.
type Document struct {
	Content  []byte
	Modified time.Time
	Version  int
	Archived bool
	// Metadata is publisher metadata as persisted: nil when there is none,
	// never an empty map, and never shared with the caller's request map.
	Metadata map[string]string
	ETag     string
	// Prune reports retention pruning performed by the write that produced
	// this document, or nil when no pruning ran. Callers on the network path
	// audit-log it so version deletions are always attributable.
	Prune *PruneResult
}

// PruneResult is the range of oldest versions a write pruned; zero when nothing
// was removed. With Err set, pruning stopped early and the survivors are still
// a contiguous, verifiable suffix.
type PruneResult struct {
	From int
	To   int
	Err  error
}

// VersionInfo describes a single version of a document.
type VersionInfo struct {
	Version  int
	Modified time.Time
}

// ErrArchived is returned by Write when the document is archived.
var ErrArchived = fmt.Errorf("document is archived")

// ErrNotModified is returned by Write when the content is identical
// to the current version, making the publish a no-op.
var ErrNotModified = fmt.Errorf("content not modified")

// ErrConflict is returned by WriteVersion when the expected version
// does not match the current version (optimistic concurrency check).
var ErrConflict = fmt.Errorf("version conflict")

// ErrVersionExists is returned by Write when the computed next version
// file already exists (O_EXCL race with a concurrent writer).
var ErrVersionExists = fmt.Errorf("version already exists")

// ErrSizeLimit is returned when combined content exceeds protocol.MaxBodyLength.
var ErrSizeLimit = fmt.Errorf("combined content exceeds size limit")

// ErrInvalidPath is returned when a publish path does not end in .md. The
// protocol serves markdown documents; a non-.md path is not a document (SPEC 2.3).
var ErrInvalidPath = fmt.Errorf("publish path must end in .md")

// ErrInvalidContent is returned when a document body is not valid UTF-8.
// Markdown is UTF-8 text; binary content is not a markdown document (SPEC 2.3, 4.4).
var ErrInvalidContent = fmt.Errorf("document body must be valid UTF-8 text")

// ErrInvalidMeta is returned when publisher metadata breaks the key, value,
// count, or size rules. Writes surface it past a caller's pre-check, since
// APPEND's merge (SPEC 6.6) can breach caps both sides satisfied alone.
var ErrInvalidMeta = fmt.Errorf("invalid publisher metadata")

// ErrPathCollision means the path is taken by the other kind: a document where
// a directory exists, or a document beneath another document.
var ErrPathCollision = errors.New("path collision")

// ErrIntegrity marks verified stored state whose bytes or hash chain are corrupt.
var ErrIntegrity = fmt.Errorf("stored version integrity failure")

// MaxSkippedSample bounds the entries a PartialWalkError retains; a badly
// damaged root must not exhaust memory during startup.
const MaxSkippedSample = 32

// SkippedEntry is a content-root entry a walk could not index.
type SkippedEntry struct {
	Path string
	Err  error
}

// PartialWalkError reports a walk that completed but skipped entries: the
// derived index is missing them. Callers log it rather than treating the
// build as whole or aborting. Skipped holds at most MaxSkippedSample entries.
type PartialWalkError struct {
	Total   int
	Skipped []SkippedEntry
}

func (e *PartialWalkError) Error() string {
	if len(e.Skipped) == 0 {
		return fmt.Sprintf("walk skipped %d entries", e.Total)
	}
	return fmt.Sprintf("walk skipped %d entries (first %s: %v)", e.Total, e.Skipped[0].Path, e.Skipped[0].Err)
}

// CurrentDoc describes a current, non-archived document version surfaced by
// WalkCurrent. Body is the markdown content with store frontmatter stripped;
// Metadata is the publisher metadata as a bare-keyed map (see extractMetadata).
type CurrentDoc struct {
	Path     string
	Body     []byte
	Metadata map[string]string
	Modified time.Time
}

// DirEntry is one entry in a directory listing. It is backend-neutral: a
// store implementation that is not a filesystem can still produce it.
type DirEntry struct {
	Name  string
	IsDir bool
}

// ListOptions selects a window of a directory listing. Entries are ordered by
// name; After skips names up to and including it, Limit 0 means no limit.
type ListOptions struct {
	IncludeArchived bool
	After           string
	Limit           int
}

// PreparedWrite is a write as it would be stored: canonical path, final body,
// persisted metadata.
type PreparedWrite struct {
	Path     string
	Content  []byte
	Metadata map[string]string
}

// WriteCheck judges a prepared write after the conflict, archive and no-op
// checks and before anything is stored; its error refuses the write.
type WriteCheck func(PreparedWrite) error

// WriteSpec is one versioned write. A negative ExpectedVersion skips the
// version check; Check may be nil.
type WriteSpec struct {
	Path            string
	ExpectedVersion int
	Content         []byte
	Metadata        map[string]string
	Check           WriteCheck
}
