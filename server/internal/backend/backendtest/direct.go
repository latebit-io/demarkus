// Package backendtest holds test helpers over the backend contract.
package backendtest

import (
	"context"
	"errors"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// Direct drives a backend.Store one call at a time: each read opens a view,
// reads and closes it. Suites use it so a case reads like the operation it
// tests; everything still goes through the contract.
type Direct struct {
	backend.Store
}

// read runs fn against a fresh view and reports a close failure with it.
func read[T any](d Direct, fn func(context.Context, backend.ReadView) (T, error)) (T, error) {
	ctx := context.Background()
	view, err := d.OpenReadView(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	value, err := fn(ctx, view)
	return value, errors.Join(err, view.Close())
}

// Get reads one version through a fresh view.
func (d Direct) Get(reqPath string, version int) (*storefmt.Document, error) {
	return read(d, func(ctx context.Context, view backend.ReadView) (*storefmt.Document, error) {
		return view.Get(ctx, reqPath, version)
	})
}

// ListEntries lists one directory through a fresh view.
func (d Direct) ListEntries(reqPath string, includeArchived bool) ([]storefmt.DirEntry, error) {
	return read(d, func(ctx context.Context, view backend.ReadView) ([]storefmt.DirEntry, error) {
		return view.ListEntries(ctx, reqPath, storefmt.ListOptions{IncludeArchived: includeArchived})
	})
}

// ListPage lists one window of a directory through a fresh view.
func (d Direct) ListPage(reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	return read(d, func(ctx context.Context, view backend.ReadView) ([]storefmt.DirEntry, error) {
		return view.ListEntries(ctx, reqPath, opts)
	})
}

// IsDir checks topology through a fresh view.
func (d Direct) IsDir(reqPath string) (bool, error) {
	return read(d, func(ctx context.Context, view backend.ReadView) (bool, error) {
		return view.IsDir(ctx, reqPath)
	})
}

// Versions reads retained history through a fresh view.
func (d Direct) Versions(reqPath string) ([]storefmt.VersionInfo, error) {
	return read(d, func(ctx context.Context, view backend.ReadView) ([]storefmt.VersionInfo, error) {
		return view.Versions(ctx, reqPath)
	})
}

// LookupHash resolves a body hash through a fresh view.
func (d Direct) LookupHash(hash string) (string, error) {
	return read(d, func(ctx context.Context, view backend.ReadView) (string, error) {
		return view.LookupHash(ctx, hash)
	})
}

// VerifyChain checks the hash chain through a fresh view.
func (d Direct) VerifyChain(reqPath string) error {
	_, err := read(d, func(ctx context.Context, view backend.ReadView) (struct{}, error) {
		return struct{}{}, view.VerifyChain(ctx, reqPath)
	})
	return err
}

// Lookup queries the catalog through a fresh view.
func (d Direct) Lookup(query string, opts catalog.Options) ([]catalog.Result, error) {
	return read(d, func(ctx context.Context, view backend.ReadView) ([]catalog.Result, error) {
		return view.Lookup(ctx, query, opts)
	})
}

// CurrentVersion is the newest retained version, 0 when the path holds none.
func (d Direct) CurrentVersion(reqPath string) (int, error) {
	versions, err := d.Versions(reqPath)
	if errors.Is(err, backend.ErrNotFound) {
		return 0, nil
	}
	current := 0
	for _, v := range versions {
		current = max(current, v.Version)
	}
	return current, err
}

// WriteVersion publishes with positional arguments.
func (d Direct) WriteVersion(reqPath string, expected int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	return d.Publish(context.Background(), backend.WriteRequest{Path: reqPath, ExpectedVersion: expected, Content: content, Metadata: meta})
}

// AppendVersion appends with positional arguments.
func (d Direct) AppendVersion(reqPath string, expected int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	return d.Append(context.Background(), backend.WriteRequest{Path: reqPath, ExpectedVersion: expected, Content: content, Metadata: meta})
}

// Archive sets archive state with a background context.
func (d Direct) Archive(reqPath string, archived bool) (backend.ArchiveResult, error) {
	return d.SetArchived(context.Background(), reqPath, archived)
}
