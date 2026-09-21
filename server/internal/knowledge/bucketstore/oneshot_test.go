package bucketstore

import (
	"context"
	"log/slog"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// The methods below let a test name one operation per line; each goes through
// the contract with a background context.

func (store *Store) direct() backendtest.Direct { return backendtest.Direct{Store: store} }

func (store *Store) Get(reqPath string, version int) (*storefmt.Document, error) {
	return store.direct().Get(reqPath, version)
}

func (store *Store) ListEntries(reqPath string, includeArchived bool) ([]storefmt.DirEntry, error) {
	return store.direct().ListEntries(reqPath, includeArchived)
}

func (store *Store) IsDir(reqPath string) (bool, error) { return store.direct().IsDir(reqPath) }

func (store *Store) Versions(reqPath string) ([]storefmt.VersionInfo, error) {
	return store.direct().Versions(reqPath)
}

func (store *Store) LookupHash(hash string) (string, error) {
	return store.direct().LookupHash(hash)
}

func (store *Store) VerifyChain(reqPath string) error { return store.direct().VerifyChain(reqPath) }

func (store *Store) Lookup(query string, options catalog.Options) ([]catalog.Result, error) {
	return store.direct().Lookup(query, options)
}

// CurrentVersionResult reads the snapshot entry, so an integrity failure on
// open surfaces here as it does for every other read.
func (store *Store) CurrentVersionResult(reqPath string) (int, error) {
	view, err := store.openReadView()
	if err != nil {
		return 0, err
	}
	return view.currentVersion(reqPath)
}

func writeRequest(path string, expected int, content []byte, metadata map[string]string) backend.WriteRequest {
	return backend.WriteRequest{Path: path, ExpectedVersion: expected, Content: content, Metadata: metadata}
}

func (store *Store) WriteVersion(path string, expected int, content []byte, metadata map[string]string) (*storefmt.Document, error) {
	return store.direct().WriteVersion(path, expected, content, metadata)
}

func (store *Store) WriteVersionResult(path string, expected int, content []byte, metadata map[string]string) (MutationResult, error) {
	return store.publish(context.Background(), writeRequest(path, expected, content, metadata))
}

func (store *Store) AppendVersion(path string, expected int, content []byte, metadata map[string]string) (*storefmt.Document, error) {
	return store.direct().AppendVersion(path, expected, content, metadata)
}

func (store *Store) AppendResult(path string, expected int, content []byte, metadata map[string]string) (MutationResult, error) {
	return store.appendVersion(context.Background(), writeRequest(path, expected, content, metadata))
}

func (store *Store) ArchiveResult(path string, archived bool) (*storefmt.Document, bool, error) {
	result, err := store.direct().Archive(path, archived)
	return result.Document, result.Changed, err
}

// openReadView pins the internal reader tests inspect directly.
func (store *Store) openReadView() (*readView, error) {
	ctx := context.Background()
	loaded, err := store.refreshSnapshot(ctx)
	if err != nil {
		return nil, normalizeReadIntegrity(err)
	}
	return &readView{ctx: ctx, objects: store.objects, snapshot: loaded}, nil
}

func (*readView) Close() error { return nil }

var discardLogger = slog.New(slog.DiscardHandler)

// enforced puts the policy decorator over a store, as the knowledge server does.
func enforced(store *Store, require bool) backendtest.Direct {
	return backendtest.Direct{Store: writepolicy.Enforce(store, writepolicy.Options{Require: require})}
}

func (view *readView) ListEntries(reqPath string, includeArchived bool) ([]storefmt.DirEntry, error) {
	return view.listPage(reqPath, storefmt.ListOptions{IncludeArchived: includeArchived})
}
