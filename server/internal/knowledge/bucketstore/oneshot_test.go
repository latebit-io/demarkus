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
	ctx := context.Background()
	loaded, err := store.refreshSnapshot(ctx)
	if err != nil {
		return 0, normalizeReadIntegrity(err)
	}
	return (&readView{objects: store.objects, snapshot: loaded}).currentVersion(ctx, reqPath)
}

// currentVersion is the snapshot entry's version, 0 for a path it lacks.
func (view *readView) currentVersion(ctx context.Context, reqPath string) (int, error) {
	logicalPath, err := view.logicalPath(ctx, reqPath)
	if err != nil {
		return 0, err
	}
	entry, exists := view.snapshot.Paths[logicalPath]
	if !exists {
		return 0, nil
	}
	return entry.Current, nil
}

func (store *Store) WriteVersion(path string, expected int, content []byte, metadata map[string]string) (*storefmt.Document, error) {
	return store.direct().WriteVersion(path, expected, content, metadata)
}

func (store *Store) AppendVersion(path string, expected int, content []byte, metadata map[string]string) (*storefmt.Document, error) {
	return store.direct().AppendVersion(path, expected, content, metadata)
}

func (store *Store) ArchiveResult(path string, archived bool) (*storefmt.Document, bool, error) {
	result, err := store.direct().Archive(path, archived)
	return result.Document, result.Changed, err
}

// pinnedView reads one contract view without naming a context per call.
type pinnedView struct {
	ctx  context.Context
	view backend.ReadView
}

// pinnedKey marks the context a pinnedView reads under, so a test can tell it
// reached the blob store.
type pinnedKey struct{}

func (store *Store) openReadView() (*pinnedView, error) {
	ctx := context.WithValue(context.Background(), pinnedKey{}, true)
	view, err := store.OpenReadView(ctx)
	if err != nil {
		return nil, err
	}
	return &pinnedView{ctx: ctx, view: view}, nil
}

func (p *pinnedView) Get(reqPath string, version int) (*storefmt.Document, error) {
	return p.view.Get(p.ctx, reqPath, version)
}

func (p *pinnedView) ListEntries(reqPath string, includeArchived bool) ([]storefmt.DirEntry, error) {
	return p.view.ListEntries(p.ctx, reqPath, storefmt.ListOptions{IncludeArchived: includeArchived})
}

func (p *pinnedView) IsDir(reqPath string) (bool, error) { return p.view.IsDir(p.ctx, reqPath) }

func (p *pinnedView) Versions(reqPath string) ([]storefmt.VersionInfo, error) {
	return p.view.Versions(p.ctx, reqPath)
}

func (p *pinnedView) LookupHash(hash string) (string, error) { return p.view.LookupHash(p.ctx, hash) }

func (p *pinnedView) VerifyChain(reqPath string) error { return p.view.VerifyChain(p.ctx, reqPath) }

func (p *pinnedView) Lookup(query string, options catalog.Options) ([]catalog.Result, error) {
	return p.view.Lookup(p.ctx, query, options)
}

func (p *pinnedView) Close() error { return p.view.Close() }

// pinned is the snapshot the view holds, for tests that compare identities.
func (p *pinnedView) pinned() *snapshot { return p.view.(*snapshotView).snapshot }

var discardLogger = slog.New(slog.DiscardHandler)

// enforced puts the policy decorator over a store, as the knowledge server does.
func enforced(store *Store, require bool) backendtest.Direct {
	return backendtest.Direct{Store: writepolicy.Enforce(store, writepolicy.Options{Require: require})}
}
