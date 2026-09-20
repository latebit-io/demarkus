// Package backend defines the storage contracts shared by handlers and stores.
package backend

import (
	"errors"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
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

// Reader exposes one committed document-store snapshot.
type Reader interface {
	Get(reqPath string, version int) (*protocolstore.Document, error)
	// ListEntries returns unique immediate children in strictly increasing Name order.
	ListEntries(reqPath string, includeArchived bool) ([]protocolstore.DirEntry, error)
	IsDir(reqPath string) (bool, error)
	Versions(reqPath string) ([]protocolstore.VersionInfo, error)
	LookupHashResult(hash string) (string, error)
	VerifyChain(reqPath string) error
}

// Store is the mutable document-store contract.
type Store interface {
	Reader
	CurrentVersionResult(reqPath string) (int, error)
	WriteVersion(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*protocolstore.Document, error)
	Append(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*protocolstore.Document, error)
	ArchiveResult(reqPath string, archived bool) (*protocolstore.Document, bool, error)
}

// CatalogReader exposes LOOKUP against the same snapshot as Reader.
type CatalogReader interface {
	Lookup(query string, opts catalog.Options) ([]catalog.Result, error)
}

// Catalog is the mutable LOOKUP contract.
type Catalog interface {
	CatalogReader
	Put(docPath string, meta map[string]string, body []byte, modified time.Time)
	Remove(docPath string)
}

// ReadView pins all read surfaces to one committed backend snapshot.
type ReadView interface {
	Reader
	CatalogReader
	Close() error
}

// ViewProvider opens one request-scoped snapshot.
type ViewProvider interface {
	OpenReadView() (ReadView, error)
}
