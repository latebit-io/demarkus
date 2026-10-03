package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// OpenReadView starts a view; its first read pins one snapshot, the log's
// tip for a read by path or one confirmed within SearchFreshness for a
// catalog read. Reads on the view share the request timeout started here.
func (store *Store) OpenReadView(ctx context.Context) (backend.ReadView, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open read view: %w", err)
	}
	return &snapshotView{store: store, objects: store.objects, deadline: time.Now().Add(store.requestTimeout)}, nil
}

// boundedBy derives a context only when ctx would outlive the deadline; a
// request context usually carries an earlier one already.
func boundedBy(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if current, ok := ctx.Deadline(); ok && !current.After(deadline) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

// snapshotView is the contract view: one snapshot pinned at its first read,
// a context per call.
type snapshotView struct {
	store    *Store // nil when the snapshot is lent pinned
	objects  objectGetter
	deadline time.Time
	closed   atomic.Bool

	mu       sync.Mutex
	snapshot *snapshot
	// deps records a commit attempt's reads; nil for every other view.
	deps *dependencies
}

var _ backend.ReadView = (*snapshotView)(nil)

// Close ends the view; the immutable snapshot itself needs no release.
func (view *snapshotView) Close() error {
	view.closed.Store(true)
	return nil
}

// pin returns the view's snapshot, choosing it on the first read.
func (view *snapshotView) pin(ctx context.Context, class readClass) (*snapshot, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.snapshot == nil {
		pinned, err := view.store.current(ctx, class)
		if err != nil {
			return nil, err
		}
		view.snapshot = pinned
	}
	return view.snapshot, nil
}

// viewRead runs one read against the pinned snapshot under the call's context.
// A closed view answers backend.ErrViewClosed.
func viewRead[T any](ctx context.Context, view *snapshotView, class readClass, fn func(context.Context, *readView) (T, error)) (T, error) {
	var zero T
	if view.closed.Load() {
		return zero, backend.ErrViewClosed
	}
	callCtx, cancel := boundedBy(ctx, view.deadline)
	defer cancel()
	if err := callCtx.Err(); err != nil {
		return zero, err
	}
	pinned, err := view.pin(callCtx, class)
	if err != nil {
		return zero, fmt.Errorf("pin snapshot: %w", normalizeReadIntegrity(err))
	}
	return fn(callCtx, &readView{objects: view.objects, snapshot: pinned, deps: view.deps})
}

func (view *snapshotView) Get(ctx context.Context, reqPath string, version int) (*storefmt.Document, error) {
	return viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) (*storefmt.Document, error) {
		return r.Get(ctx, reqPath, version)
	})
}

func (view *snapshotView) ListEntries(ctx context.Context, reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	return viewRead(ctx, view, catalogRead, func(ctx context.Context, r *readView) ([]storefmt.DirEntry, error) {
		return r.listPage(ctx, reqPath, opts)
	})
}

// IsDir is exact: FETCH asks it before reading a version path.
func (view *snapshotView) IsDir(ctx context.Context, reqPath string) (bool, error) {
	return viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) (bool, error) { return r.IsDir(ctx, reqPath) })
}

func (view *snapshotView) Versions(ctx context.Context, reqPath string) ([]storefmt.VersionInfo, error) {
	return viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) ([]storefmt.VersionInfo, error) {
		return r.Versions(ctx, reqPath)
	})
}

func (view *snapshotView) LookupHash(ctx context.Context, hash string) (string, error) {
	return viewRead(ctx, view, catalogRead, func(ctx context.Context, r *readView) (string, error) { return r.LookupHash(ctx, hash) })
}

func (view *snapshotView) VerifyChain(ctx context.Context, reqPath string) error {
	_, err := viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) (struct{}, error) {
		return struct{}{}, r.VerifyChain(ctx, reqPath)
	})
	return err
}

// Lookup in body mode answers from the section index version built for the
// pinned snapshot, waiting briefly for it; without one it refuses with
// backend.ErrBodyMatchUnavailable and the caller answers from the catalog.
func (view *snapshotView) Lookup(ctx context.Context, query string, options catalog.Options) ([]catalog.Result, error) {
	return viewRead(ctx, view, catalogRead, func(ctx context.Context, r *readView) ([]catalog.Result, error) {
		if options.Match != catalog.MatchBody {
			return search(ctx, r.scan(), query, options)
		}
		if view.store == nil {
			return nil, backend.ErrBodyMatchUnavailable
		}
		version, ok := view.store.sections.await(ctx, r.snapshot.Sequence)
		if !ok {
			return nil, backend.ErrBodyMatchUnavailable
		}
		return search(ctx, version, query, options)
	})
}

// attemptView lends a commit attempt's snapshot to a precondition as an
// ordinary contract view, bounded by the attempt's own deadline; its reads
// join the attempt's dependencies.
func attemptView(ctx context.Context, view *readView) *snapshotView {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultRequestTimeout)
	}
	return &snapshotView{objects: view.objects, deadline: deadline, snapshot: view.snapshot, deps: view.deps}
}

// readView reads one snapshot; every method takes the operation's context.
// deps is set while a commit attempt builds on it.
type readView struct {
	objects  objectGetter
	snapshot *snapshot
	deps     *dependencies
}

// path, isDirectory and scan read the snapshot, recording each read for a
// commit attempt's rebase.
func (view *readView) path(path string) *pathState {
	state := view.snapshot.path(path)
	view.deps.sawPath(path, state)
	return state
}

func (view *readView) isDirectory(path string) bool {
	isDir := view.snapshot.isDirectory(path)
	view.deps.sawDirectory(path, isDir)
	return isDir
}

// scan is the whole snapshot, for a listing or catalog read any change alters.
func (view *readView) scan() *snapshot {
	view.deps.sawAll()
	return view.snapshot
}

type retainedVersion struct {
	entry    historyEntry
	modified time.Time
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
	current := version == 0
	requested := version
	if requested == 0 {
		requested = entry.Current
	}
	retained, ok, err := view.retained(ctx, entry, requested)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, backend.ErrNotFound
	}
	raw, stored, err := view.loadStored(ctx, &retained)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return documentFromRetained(raw, &retained, &stored, current && entry.Archived), nil
}

// loadStored reads a retained version's bytes and checks them against it.
func (view *readView) loadStored(ctx context.Context, retained *retainedVersion) ([]byte, storedDocument, error) {
	raw, err := view.loadBlob(ctx, retained.entry.Blob)
	if err != nil {
		return nil, storedDocument{}, fmt.Errorf("load v%d: %w", retained.entry.Version, err)
	}
	stored, err := validateStoredDocument(raw, retained)
	return raw, stored, err
}

// listPage windows a directory; its names come in order, so After is a seek
// and Limit an early stop.
func (view *readView) listPage(ctx context.Context, reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	includeArchived := opts.IncludeArchived
	logicalPath, err := view.logicalPath(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	if view.path(logicalPath) != nil || !view.isDirectory(logicalPath) {
		return nil, backend.ErrNotFound
	}
	var entries []storefmt.DirEntry
	view.scan().children(logicalPath, opts.After, func(child *dirChild) bool {
		if opts.Limit > 0 && len(entries) == opts.Limit {
			return false
		}
		if hiddenLogicalName(child.Name) || (includeArchived && child.Visible == 0) || (!includeArchived && child.Live == 0) {
			return true
		}
		entries = append(entries, storefmt.DirEntry{Name: child.Name, IsDir: child.IsDir})
		return true
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []storefmt.DirEntry{}
	}
	return entries, nil
}

func (view *readView) IsDir(ctx context.Context, reqPath string) (bool, error) {
	logicalPath, err := view.logicalPath(ctx, reqPath)
	if err != nil {
		return false, err
	}
	if view.isDirectory(logicalPath) {
		return true, nil
	}
	if view.path(logicalPath) != nil {
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
	history, err := view.history(ctx, entry)
	if err != nil {
		return nil, err
	}
	versions := make([]storefmt.VersionInfo, 0, len(history))
	for _, retained := range slices.Backward(history) {
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
	path, exists := view.scan().lookupHash(hash)
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
	history, err := view.history(ctx, entry)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	previous := history[0]
	previousRaw, err := view.loadBlobUnchecked(ctx, previous.entry.Blob)
	if err != nil {
		return fmt.Errorf("read v%d: %w", previous.entry.Version, err)
	}
	for _, current := range history[1:] {
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
		if err := validateVersion(previousRaw, &previous); err != nil {
			return err
		}
		previous = current
		previousRaw = currentRaw
	}
	if err := validateVersion(previousRaw, &previous); err != nil {
		return err
	}
	return ctx.Err()
}

// validateVersion checks a version's stored bytes against its log record.
func validateVersion(raw []byte, retained *retainedVersion) error {
	if _, err := validateStoredDocument(raw, retained); err != nil {
		return fmt.Errorf("validate v%d: %w", retained.entry.Version, err)
	}
	if err := verifyBlobHash(retained.entry.Blob, raw); err != nil {
		return fmt.Errorf("validate v%d: %w", retained.entry.Version, err)
	}
	return nil
}

// search runs a lookup over index and clones what the results share with it.
func search(ctx context.Context, index catalog.Index, query string, options catalog.Options) ([]catalog.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results, err := catalog.Search(cancellable{Index: index, ctx: ctx}, query, options)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The slice is the search's own; tags and metadata are the snapshot's.
	for index := range results {
		results[index].Tags = slices.Clone(results[index].Tags)
		results[index].Metadata = maps.Clone(results[index].Metadata)
	}
	return results, nil
}

// cancellable ends an index scan soon after ctx ends, so a lookup over a
// large world stops with its request.
type cancellable struct {
	catalog.Index
	ctx context.Context
}

func (index cancellable) Entries(scope string) iter.Seq2[*catalog.Entry, *catalog.DocSections] {
	return func(yield func(*catalog.Entry, *catalog.DocSections) bool) {
		scanned := 0
		for entry, sections := range index.Index.Entries(scope) {
			scanned++
			if scanned%cancelCheck == 0 && index.ctx.Err() != nil {
				return
			}
			if !yield(entry, sections) {
				return
			}
		}
	}
}

// cancelCheck is how many entries a scan visits between context checks.
const cancelCheck = 1024

func (view *readView) documentEntry(ctx context.Context, reqPath string) (*pathState, error) {
	logicalPath, err := view.logicalPath(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	entry := view.path(logicalPath)
	if entry == nil {
		return nil, backend.ErrNotFound
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

// currentRetained is a document's current version: the newest one committed
// since the checkpoint when there is one, so reading its body costs one read.
func (view *readView) currentRetained(ctx context.Context, state *pathState) (retainedVersion, error) {
	retained, ok, err := view.retained(ctx, state, state.Current)
	if err == nil && !ok {
		err = fmt.Errorf("%w: current version %d is not retained", blob.ErrIntegrity, state.Current)
	}
	return retained, err
}

// retained is one version's record, from the snapshot's recent versions or
// the one history block that holds it: never the whole history.
func (view *readView) retained(ctx context.Context, state *pathState, version int) (retainedVersion, bool, error) {
	if version < state.First || version > state.Current {
		return retainedVersion{}, false, nil
	}
	retained, ok := retainedAt(state.Recent, version)
	if !ok {
		var err error
		if retained, err = view.baseVersion(ctx, state, version); err != nil {
			return retainedVersion{}, false, err
		}
	}
	if version == state.Current && !tipMatches([]retainedVersion{retained}, state.Current, state.BodyHash, state.Modified) {
		return retainedVersion{}, false, fmt.Errorf("%w: history tip does not match the document", blob.ErrIntegrity)
	}
	return retained, true, nil
}

// baseVersion reads version from the checkpoint history block that holds it.
func (view *readView) baseVersion(ctx context.Context, state *pathState, version int) (retainedVersion, error) {
	var refs []blockRef
	if state.Base != nil {
		refs = state.Base.History
	}
	index := slices.IndexFunc(refs, func(ref blockRef) bool { return ref.First <= version && version <= ref.Last })
	if index < 0 {
		return retainedVersion{}, fmt.Errorf("%w: no history block holds v%d of %s", blob.ErrIntegrity, version, state.Path)
	}
	block, err := readBlock(ctx, view.objects, pathHash(state.Path), refs[index])
	if err != nil {
		return retainedVersion{}, fmt.Errorf("load history %d: %w", index, err)
	}
	for _, entry := range block.Entries {
		if entry.Version == version {
			modified, err := parseTimestamp(entry.Modified)
			if err != nil {
				return retainedVersion{}, fmt.Errorf("%w: history v%d modified: %v", blob.ErrIntegrity, version, err)
			}
			return retainedVersion{entry: entry, modified: modified}, nil
		}
	}
	return retainedVersion{}, fmt.Errorf("%w: history block %d lacks v%d", blob.ErrIntegrity, index, version)
}

// history is every retained version of a document, oldest first: the
// checkpoint's history blocks for what it holds, then the versions committed
// since.
func (view *readView) history(ctx context.Context, state *pathState) ([]retainedVersion, error) {
	var versions []retainedVersion
	if state.Base != nil && state.First <= state.Base.Current {
		base, err := view.baseHistory(ctx, state)
		if err != nil {
			return nil, err
		}
		versions = retainFrom(base, state.First)
	}
	versions = append(versions, retainFrom(state.Recent, state.First)...)
	for index := 1; index < len(versions); index++ {
		if versions[index].entry.Version != versions[index-1].entry.Version+1 {
			return nil, fmt.Errorf("%w: retained versions skip from %d to %d", blob.ErrIntegrity, versions[index-1].entry.Version, versions[index].entry.Version)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: no retained versions", blob.ErrIntegrity)
	}
	if !tipMatches(versions, state.Current, state.BodyHash, state.Modified) {
		return nil, fmt.Errorf("%w: history tip does not match the document", blob.ErrIntegrity)
	}
	return versions, nil
}

// baseHistory reads the versions a document's checkpoint entry retains.
func (view *readView) baseHistory(ctx context.Context, state *pathState) ([]retainedVersion, error) {
	base, documentHash := state.Base, pathHash(state.Path)
	var versions []retainedVersion
	for index, ref := range base.History {
		history, err := readBlock(ctx, view.objects, documentHash, ref)
		if err != nil {
			return nil, fmt.Errorf("load history %d: %w", index, err)
		}
		for _, historyEntry := range history.Entries {
			modified, err := parseTimestamp(historyEntry.Modified)
			if err != nil {
				return nil, fmt.Errorf("%w: history v%d modified: %v", blob.ErrIntegrity, historyEntry.Version, err)
			}
			versions = append(versions, retainedVersion{entry: historyEntry, modified: modified})
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: checkpoint entry has no retained versions", blob.ErrIntegrity)
	}
	if !tipMatches(versions, base.Current, base.BodyHash, base.Modified) {
		return nil, fmt.Errorf("%w: history tip does not match checkpoint entry", blob.ErrIntegrity)
	}
	return versions, nil
}

// tipMatches reports whether versions end at the version recorded as current.
func tipMatches(versions []retainedVersion, current int, bodyHash string, modified time.Time) bool {
	tip := versions[len(versions)-1]
	return tip.entry.Version == current && tip.entry.BodyHash == bodyHash && tip.modified.Equal(modified)
}

// readBlock reads one of a document's history blocks.
func readBlock(ctx context.Context, objects objectGetter, documentHash string, ref blockRef) (historyObject, error) {
	key := historyKey(ref.Hash)
	return getImmutable(ctx, objects, keyedRef{objectRef{Key: key, Hash: ref.Hash}, key}, func(history *historyObject) error {
		if err := validateHistoryObject(history); err != nil {
			return err
		}
		if history.PathHash != documentHash || history.First != ref.First || history.Last != ref.Last {
			return fmt.Errorf("object does not match history reference")
		}
		return nil
	})
}

func (view *readView) loadBlob(ctx context.Context, ref objectRef) ([]byte, error) {
	return getVerified(ctx, view.objects, keyedRef{ref, blobKey(ref.Hash)})
}

func (view *readView) loadBlobUnchecked(ctx context.Context, ref objectRef) ([]byte, error) {
	return getReferenced(ctx, view.objects, keyedRef{ref, blobKey(ref.Hash)})
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
