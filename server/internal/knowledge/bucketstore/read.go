package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

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
	raw, stored, err := view.loadStored(ctx, entry.Path, &retained)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return documentFromRetained(raw, &retained, &stored, current && entry.Archived), nil
}

// loadStored reads a retained version of the document at docPath and checks
// its bytes against the record.
func (view *readView) loadStored(ctx context.Context, docPath string, retained *retainedVersion) ([]byte, storedDocument, error) {
	raw, err := view.loadBlob(ctx, retained.entry.Blob)
	if err != nil {
		return nil, storedDocument{}, fmt.Errorf("load v%d: %w", retained.entry.Version, err)
	}
	stored, err := validateStoredDocument(raw, docPath, retained)
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
		if err := validateVersion(previousRaw, entry.Path, &previous); err != nil {
			return err
		}
		previous = current
		previousRaw = currentRaw
	}
	if err := validateVersion(previousRaw, entry.Path, &previous); err != nil {
		return err
	}
	return ctx.Err()
}

// validateVersion checks a version's stored bytes against its log record.
func validateVersion(raw []byte, docPath string, retained *retainedVersion) error {
	if _, err := validateStoredDocument(raw, docPath, retained); err != nil {
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
	versions = append(versions, state.Recent...)
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

// retainFrom drops the versions retention pruned below first.
func retainFrom(versions []retainedVersion, first int) []retainedVersion {
	index := 0
	for index < len(versions) && versions[index].entry.Version < first {
		index++
	}
	return versions[index:]
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
	object, err := getVerified(ctx, view.objects, keyedRef{ref, blobKey(ref.Hash)})
	return object.Data, err
}

func (view *readView) loadBlobUnchecked(ctx context.Context, ref objectRef) ([]byte, error) {
	object, err := getReferenced(ctx, view.objects, keyedRef{ref, blobKey(ref.Hash)})
	return object.Data, err
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

// validateStoredDocument checks raw against its record and the document at
// docPath; a version written before the store recorded paths names none.
func validateStoredDocument(raw []byte, docPath string, retained *retainedVersion) (storedDocument, error) {
	version := retained.entry.Version
	header, err := storefmt.InspectStoredVersion(raw)
	if err == nil {
		err = header.CheckNames(docPath, version)
	}
	if err != nil {
		return storedDocument{}, fmt.Errorf("%w: v%d stored version: %v", blob.ErrIntegrity, version, err)
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
