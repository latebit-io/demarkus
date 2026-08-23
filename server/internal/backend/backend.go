// Package backend defines the storage contracts shared by handlers and stores.
package backend

import (
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// Reader exposes one committed document-store snapshot.
type Reader interface {
	Get(reqPath string, version int) (*protocolstore.Document, error)
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
