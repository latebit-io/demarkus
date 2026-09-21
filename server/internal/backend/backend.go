// Package backend defines the storage contracts shared by handlers and stores.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// Refusals a backend reports without the handler knowing the backend.
var (
	// ErrQuota means the write would pass a configured limit.
	ErrQuota = errors.New("quota exceeded")
	// ErrRejected means the store refused a write the publisher can correct.
	ErrRejected = errors.New("write rejected")
)

// Rejection is an ErrRejected that carries its reason for the response body.
type Rejection interface {
	error
	RejectionMessage() string
}

// ErrNotFound means the path, version or hash names nothing a reader may see.
var ErrNotFound = errors.New("not found")

// FromNotExist marks a backend's own missing-file error as ErrNotFound, keeping
// the cause in the chain.
func FromNotExist(err error) error {
	if err != nil && errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// Reader exposes one committed document-store snapshot.
type Reader interface {
	Get(ctx context.Context, reqPath string, version int) (*storefmt.Document, error)
	// ListEntries returns unique immediate children in strictly increasing Name order.
	ListEntries(ctx context.Context, reqPath string, includeArchived bool) ([]storefmt.DirEntry, error)
	IsDir(ctx context.Context, reqPath string) (bool, error)
	Versions(ctx context.Context, reqPath string) ([]storefmt.VersionInfo, error)
	LookupHash(ctx context.Context, hash string) (string, error)
	VerifyChain(ctx context.Context, reqPath string) error
}

// WriteRequest is one PUBLISH or APPEND. ExpectedVersion is the version the
// writer last saw; a negative value skips the check.
type WriteRequest struct {
	Path            string
	ExpectedVersion int
	Content         []byte
	Metadata        map[string]string
}

// ArchiveResult is the document after an archive transition; Changed is false
// when it already had the requested state.
type ArchiveResult struct {
	Document *storefmt.Document
	Changed  bool
}

// Store is the document-store contract: reads go through a view, writes are
// atomic and keep the LOOKUP catalog current.
type Store interface {
	ViewProvider
	Publish(ctx context.Context, req WriteRequest) (*storefmt.Document, error)
	Append(ctx context.Context, req WriteRequest) (*storefmt.Document, error)
	SetArchived(ctx context.Context, reqPath string, archived bool) (ArchiveResult, error)
}

// CatalogReader exposes LOOKUP against the same snapshot as Reader.
type CatalogReader interface {
	Lookup(ctx context.Context, query string, opts catalog.Options) ([]catalog.Result, error)
}

// ReadView pins all read surfaces to one committed backend snapshot.
type ReadView interface {
	Reader
	CatalogReader
	Close() error
}

// ViewProvider opens one request-scoped snapshot; ctx bounds acquiring it.
type ViewProvider interface {
	OpenReadView(ctx context.Context) (ReadView, error)
}
