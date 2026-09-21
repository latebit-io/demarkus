package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// OpenReadView validates the current head and pins one immutable snapshot.
// Reads on the view share the request timeout that started here.
func (store *Store) OpenReadView(ctx context.Context) (backend.ReadView, error) {
	deadline := time.Now().Add(store.requestTimeout)
	openCtx, cancel := boundedBy(ctx, deadline)
	defer cancel()
	loaded, err := store.refreshSnapshot(openCtx)
	if err != nil {
		return nil, fmt.Errorf("open read view: %w", normalizeReadIntegrity(err))
	}
	return &snapshotView{objects: store.objects, snapshot: loaded, deadline: deadline}, nil
}

// boundedBy derives a context only when ctx would outlive the deadline; a
// request context usually carries an earlier one already.
func boundedBy(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if current, ok := ctx.Deadline(); ok && !current.After(deadline) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

// snapshotView is the contract view: one pinned snapshot, a context per call.
type snapshotView struct {
	objects  blob.Store
	snapshot *snapshot
	deadline time.Time
	closed   atomic.Bool
}

var _ backend.ReadView = (*snapshotView)(nil)

// Close ends the view; the immutable snapshot itself needs no release.
func (view *snapshotView) Close() error {
	view.closed.Store(true)
	return nil
}

// viewRead runs one read against the pinned snapshot under the call's context.
// A closed view answers backend.ErrViewClosed.
func viewRead[T any](ctx context.Context, view *snapshotView, fn func(context.Context, *readView) (T, error)) (T, error) {
	if view.closed.Load() {
		var zero T
		return zero, backend.ErrViewClosed
	}
	callCtx, cancel := boundedBy(ctx, view.deadline)
	defer cancel()
	return fn(callCtx, &readView{objects: view.objects, snapshot: view.snapshot})
}

func (view *snapshotView) Get(ctx context.Context, reqPath string, version int) (*storefmt.Document, error) {
	return viewRead(ctx, view, func(ctx context.Context, r *readView) (*storefmt.Document, error) {
		return r.Get(ctx, reqPath, version)
	})
}

func (view *snapshotView) ListEntries(ctx context.Context, reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	return viewRead(ctx, view, func(ctx context.Context, r *readView) ([]storefmt.DirEntry, error) {
		return r.listPage(ctx, reqPath, opts)
	})
}

func (view *snapshotView) IsDir(ctx context.Context, reqPath string) (bool, error) {
	return viewRead(ctx, view, func(ctx context.Context, r *readView) (bool, error) { return r.IsDir(ctx, reqPath) })
}

func (view *snapshotView) Versions(ctx context.Context, reqPath string) ([]storefmt.VersionInfo, error) {
	return viewRead(ctx, view, func(ctx context.Context, r *readView) ([]storefmt.VersionInfo, error) {
		return r.Versions(ctx, reqPath)
	})
}

func (view *snapshotView) LookupHash(ctx context.Context, hash string) (string, error) {
	return viewRead(ctx, view, func(ctx context.Context, r *readView) (string, error) { return r.LookupHash(ctx, hash) })
}

func (view *snapshotView) VerifyChain(ctx context.Context, reqPath string) error {
	_, err := viewRead(ctx, view, func(ctx context.Context, r *readView) (struct{}, error) {
		return struct{}{}, r.VerifyChain(ctx, reqPath)
	})
	return err
}

func (view *snapshotView) Lookup(ctx context.Context, query string, options catalog.Options) ([]catalog.Result, error) {
	return viewRead(ctx, view, func(ctx context.Context, r *readView) ([]catalog.Result, error) { return r.Lookup(ctx, query, options) })
}

// attemptView lends a commit attempt's snapshot to a precondition as an
// ordinary contract view, bounded by the attempt's own deadline.
func attemptView(ctx context.Context, view *readView) *snapshotView {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultRequestTimeout)
	}
	return &snapshotView{objects: view.objects, snapshot: view.snapshot, deadline: deadline}
}

// readView reads one snapshot; every method takes the operation's context.
type readView struct {
	objects  blob.Store
	snapshot *snapshot
}

type retainedVersion struct {
	entry    historyEntry
	modified time.Time
}

type retainedHistory struct {
	manifest manifestObject
	versions []retainedVersion
}

type storedDocument struct {
	body     []byte
	metadata map[string]string
}

func (view *readView) Get(ctx context.Context, reqPath string, version int) (*storefmt.Document, error) {
	document, err := view.get(ctx, reqPath, version)
	return document, normalizeReadIntegrity(err)
}

func (view *readView) get(ctx context.Context, reqPath string, version int) (*storefmt.Document, error) {
	entry, err := view.documentEntry(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	if version < 0 {
		return nil, backend.ErrNotFound
	}
	history, err := view.loadHistory(ctx, &entry)
	if err != nil {
		return nil, err
	}
	current := version == 0
	requested := version
	if requested == 0 {
		requested = history.manifest.Current
	}
	retained, ok := retainedAt(history.versions, requested)
	if !ok {
		return nil, backend.ErrNotFound
	}
	raw, err := view.loadBlob(ctx, retained.entry.Blob)
	if err != nil {
		return nil, fmt.Errorf("load v%d: %w", retained.entry.Version, err)
	}
	stored, err := validateStoredDocument(raw, &retained)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &storefmt.Document{
		Content:  bytes.Clone(stored.body),
		Modified: retained.modified,
		Version:  retained.entry.Version,
		Archived: current && entry.Archived,
		Metadata: maps.Clone(stored.metadata),
		ETag:     storefmt.StoredETag(raw),
	}, nil
}

// listPage windows the derived directory; Children are sorted by name, so
// After is a binary search and Limit an early stop.
func (view *readView) listPage(ctx context.Context, reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	includeArchived := opts.IncludeArchived
	logicalPath, err := view.logicalPath(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	if _, isDocument := view.snapshot.Paths[logicalPath]; isDocument {
		return nil, backend.ErrNotFound
	}
	directory, exists := view.snapshot.Directories[logicalPath]
	if !exists {
		return nil, backend.ErrNotFound
	}
	children := directory.Children
	if opts.After != "" {
		start, found := slices.BinarySearchFunc(children, opts.After, func(child directoryChild, after string) int {
			return strings.Compare(child.Name, after)
		})
		if found {
			start++
		}
		children = children[start:]
	}
	capacity := len(children)
	if opts.Limit > 0 {
		capacity = min(capacity, opts.Limit)
	}
	entries := make([]storefmt.DirEntry, 0, capacity)
	for _, child := range children {
		if opts.Limit > 0 && len(entries) == opts.Limit {
			break
		}
		if hiddenLogicalName(child.Name) || (includeArchived && !child.Visible) || (!includeArchived && !child.Live) {
			continue
		}
		entries = append(entries, storefmt.DirEntry{Name: child.Name, IsDir: child.IsDir})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (view *readView) IsDir(ctx context.Context, reqPath string) (bool, error) {
	logicalPath, err := view.logicalPath(ctx, reqPath)
	if err != nil {
		return false, err
	}
	if _, exists := view.snapshot.Directories[logicalPath]; exists {
		return true, nil
	}
	if _, exists := view.snapshot.Paths[logicalPath]; exists {
		return false, nil
	}
	return false, backend.ErrNotFound
}

func (view *readView) Versions(ctx context.Context, reqPath string) ([]storefmt.VersionInfo, error) {
	versions, err := view.versions(ctx, reqPath)
	return versions, normalizeReadIntegrity(err)
}

func (view *readView) versions(ctx context.Context, reqPath string) ([]storefmt.VersionInfo, error) {
	entry, err := view.documentEntry(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	history, err := view.loadHistory(ctx, &entry)
	if err != nil {
		return nil, err
	}
	versions := make([]storefmt.VersionInfo, 0, len(history.versions))
	for _, retained := range slices.Backward(history.versions) {
		versions = append(versions, storefmt.VersionInfo{
			Version:  retained.entry.Version,
			Modified: retained.modified,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return versions, nil
}

func (view *readView) LookupHash(ctx context.Context, hash string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, exists := view.snapshot.BodyHashes[hash]
	if !exists {
		return "", backend.ErrNotFound
	}
	return path, nil
}

func (view *readView) VerifyChain(ctx context.Context, reqPath string) error {
	return normalizeReadIntegrity(view.verifyChain(ctx, reqPath))
}

func normalizeReadIntegrity(err error) error {
	if err != nil && errors.Is(err, blob.ErrIntegrity) && !errors.Is(err, storefmt.ErrIntegrity) {
		return errors.Join(storefmt.ErrIntegrity, err)
	}
	return err
}

func (view *readView) verifyChain(ctx context.Context, reqPath string) error {
	entry, err := view.documentEntry(ctx, reqPath)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	history, err := view.loadHistory(ctx, &entry)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	previous := history.versions[0]
	previousRaw, err := view.loadBlobUnchecked(ctx, previous.entry.Blob)
	if err != nil {
		return fmt.Errorf("read v%d: %w", previous.entry.Version, err)
	}
	for _, current := range history.versions[1:] {
		currentRaw, err := view.loadBlobUnchecked(ctx, current.entry.Blob)
		if err != nil {
			return fmt.Errorf("read v%d: %w", current.entry.Version, err)
		}
		recorded := storefmt.ExtractPreviousHash(currentRaw)
		if recorded == "" {
			return fmt.Errorf("%w: v%d missing previous-hash", blob.ErrIntegrity, current.entry.Version)
		}
		expected := "sha256-" + storefmt.StoredETag(previousRaw)
		if recorded != expected {
			return fmt.Errorf("%w: v%d chain broken: previous-hash mismatch (want %s, got %s)",
				blob.ErrIntegrity, current.entry.Version, expected, recorded)
		}
		if _, err := validateStoredDocument(previousRaw, &previous); err != nil {
			return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
		}
		if err := verifyBlobHash(previous.entry.Blob, previousRaw); err != nil {
			return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
		}
		previous = current
		previousRaw = currentRaw
	}
	if _, err := validateStoredDocument(previousRaw, &previous); err != nil {
		return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
	}
	if err := verifyBlobHash(previous.entry.Blob, previousRaw); err != nil {
		return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
	}
	return ctx.Err()
}

func (view *readView) Lookup(ctx context.Context, query string, options catalog.Options) ([]catalog.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results, err := view.snapshot.Catalog.Lookup(query, options)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cloned := slices.Clone(results)
	for index := range results {
		cloned[index].Tags = slices.Clone(results[index].Tags)
		cloned[index].Metadata = maps.Clone(results[index].Metadata)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return cloned, nil
}

func (view *readView) documentEntry(ctx context.Context, reqPath string) (snapshotEntry, error) {
	logicalPath, err := view.logicalPath(ctx, reqPath)
	if err != nil {
		return snapshotEntry{}, err
	}
	entry, exists := view.snapshot.Paths[logicalPath]
	if !exists {
		return snapshotEntry{}, backend.ErrNotFound
	}
	return entry, nil
}

func (view *readView) logicalPath(ctx context.Context, reqPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	relative, err := storefmt.RelPath(reqPath)
	if err != nil {
		return "", backend.FromNotExist(err)
	}
	if relative == "" {
		return "/", nil
	}
	return "/" + relative, nil
}

func (view *readView) loadHistory(ctx context.Context, entry *snapshotEntry) (retainedHistory, error) {
	manifest, err := getImmutable(ctx, view.objects, keyedRef{entry.Manifest, manifestKey(entry.PathHash, entry.Manifest.Hash)}, validateManifestObject)
	if err != nil {
		return retainedHistory{}, fmt.Errorf("load manifest: %w", err)
	}
	if manifest.PathHash != entry.PathHash || manifest.Current != entry.Current || manifest.Archived != entry.Archived {
		return retainedHistory{}, fmt.Errorf("%w: manifest does not match shard entry", blob.ErrIntegrity)
	}

	loaded := retainedHistory{manifest: manifest}
	for index, ref := range manifest.History {
		history, err := getImmutable(ctx, view.objects, keyedRef{ref.objectRef, historyKey(ref.Hash)}, func(history *historyObject) error {
			if err := validateHistoryObject(history); err != nil {
				return err
			}
			if history.PathHash != ref.PathHash || history.First != ref.First || history.Last != ref.Last {
				return fmt.Errorf("object does not match history reference")
			}
			return nil
		})
		if err != nil {
			return retainedHistory{}, fmt.Errorf("load history %d: %w", index, err)
		}
		for _, historyEntry := range history.Entries {
			modified, err := parseTimestamp(historyEntry.Modified)
			if err != nil {
				return retainedHistory{}, fmt.Errorf("%w: history v%d modified: %v", blob.ErrIntegrity, historyEntry.Version, err)
			}
			loaded.versions = append(loaded.versions, retainedVersion{entry: historyEntry, modified: modified})
		}
	}
	if len(loaded.versions) == 0 {
		return retainedHistory{}, fmt.Errorf("%w: manifest has no retained versions", blob.ErrIntegrity)
	}
	tip := loaded.versions[len(loaded.versions)-1]
	if tip.entry.Version != entry.Current || tip.entry.BodyHash != entry.BodyHash || !tip.modified.Equal(entry.Modified) {
		return retainedHistory{}, fmt.Errorf("%w: history tip does not match shard entry", blob.ErrIntegrity)
	}
	return loaded, nil
}

func (view *readView) loadBlob(ctx context.Context, ref objectRef) ([]byte, error) {
	data, err := view.loadBlobUnchecked(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := verifyBlobHash(ref, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (view *readView) loadBlobUnchecked(ctx context.Context, ref objectRef) ([]byte, error) {
	if err := verifyRef(ref, blobKey(ref.Hash)); err != nil {
		return nil, fmt.Errorf("%w: %v", blob.ErrIntegrity, err)
	}
	value, err := view.objects.Get(ctx, ref.Key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("%w: referenced object %q is missing: %w", blob.ErrIntegrity, ref.Key, err)
		}
		return nil, fmt.Errorf("read referenced object %q: %w", ref.Key, err)
	}
	if err := validateReadObject(ref.Key, &value); err != nil {
		return nil, err
	}
	return value.Data, nil
}

func verifyBlobHash(ref objectRef, data []byte) error {
	if actual := hashHex(data); actual != ref.Hash {
		return fmt.Errorf("%w: object %q hash is %s, reference is %s", blob.ErrIntegrity, ref.Key, actual, ref.Hash)
	}
	return nil
}

func retainedAt(versions []retainedVersion, version int) (retainedVersion, bool) {
	if len(versions) == 0 {
		return retainedVersion{}, false
	}
	index := version - versions[0].entry.Version
	if index < 0 || index >= len(versions) || versions[index].entry.Version != version {
		return retainedVersion{}, false
	}
	return versions[index], true
}

func validateStoredDocument(raw []byte, retained *retainedVersion) (storedDocument, error) {
	header, err := storefmt.InspectStoredVersion(raw)
	if err != nil {
		return storedDocument{}, fmt.Errorf("%w: v%d stored version: %v", blob.ErrIntegrity, retained.entry.Version, err)
	}
	version := header.Version
	if version != retained.entry.Version {
		return storedDocument{}, fmt.Errorf("%w: stored version is %d, history says %d", blob.ErrIntegrity, version, retained.entry.Version)
	}
	body := storefmt.ExtractBody(raw)
	if bodyHash := storefmt.ContentHash(body); bodyHash != retained.entry.BodyHash {
		return storedDocument{}, fmt.Errorf("%w: v%d body hash is %s, history says %s", blob.ErrIntegrity, version, bodyHash, retained.entry.BodyHash)
	}
	metadata := storefmt.ExtractMetadata(raw)
	if err := storefmt.ValidateWrite(body, metadata); err != nil {
		return storedDocument{}, fmt.Errorf("%w: v%d stored data: %v", blob.ErrIntegrity, version, err)
	}
	return storedDocument{body: body, metadata: metadata}, nil
}
