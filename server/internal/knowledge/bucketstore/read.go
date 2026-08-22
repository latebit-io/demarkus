package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// OpenReadView validates the current head and pins one immutable snapshot.
func (store *Store) OpenReadView() (backend.ReadView, error) {
	view, err := store.openReadView()
	if err != nil {
		return nil, err
	}
	return view, nil
}

func (store *Store) openReadView() (*readView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), store.requestTimeout)
	loaded, err := store.refreshSnapshot(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open read view: %w", err)
	}
	return &readView{ctx: ctx, cancel: cancel, objects: store.objects, snapshot: loaded}, nil
}

type readView struct {
	ctx      context.Context
	cancel   context.CancelFunc
	objects  blob.Store
	snapshot *snapshot
	close    sync.Once
}

var _ backend.ReadView = (*readView)(nil)

func (view *readView) Close() error {
	view.close.Do(view.cancel)
	return nil
}

// Get reads one exact version from a freshly validated snapshot.
func (store *Store) Get(reqPath string, version int) (*protocolstore.Document, error) {
	view, err := store.openReadView()
	if err != nil {
		return nil, err
	}
	document, readErr := view.Get(reqPath, version)
	return document, readResultError(readErr, view.Close())
}

// ListEntries reads one derived directory from a freshly validated snapshot.
func (store *Store) ListEntries(reqPath string, includeArchived bool) ([]protocolstore.DirEntry, error) {
	view, err := store.openReadView()
	if err != nil {
		return nil, err
	}
	entries, readErr := view.ListEntries(reqPath, includeArchived)
	return entries, readResultError(readErr, view.Close())
}

// IsDir checks derived topology in a freshly validated snapshot.
func (store *Store) IsDir(reqPath string) (bool, error) {
	view, err := store.openReadView()
	if err != nil {
		return false, err
	}
	isDirectory, readErr := view.IsDir(reqPath)
	return isDirectory, readResultError(readErr, view.Close())
}

// Versions reads retained history from a freshly validated snapshot.
func (store *Store) Versions(reqPath string) ([]protocolstore.VersionInfo, error) {
	view, err := store.openReadView()
	if err != nil {
		return nil, err
	}
	versions, readErr := view.Versions(reqPath)
	return versions, readResultError(readErr, view.Close())
}

// LookupHash resolves a live body hash in a freshly validated snapshot.
func (store *Store) LookupHash(hash string) (string, error) {
	view, err := store.openReadView()
	if err != nil {
		return "", err
	}
	path, readErr := view.LookupHash(hash)
	return path, readResultError(readErr, view.Close())
}

// VerifyChain checks retained blobs from a freshly validated snapshot.
func (store *Store) VerifyChain(reqPath string) error {
	view, err := store.openReadView()
	if err != nil {
		return err
	}
	return readResultError(view.VerifyChain(reqPath), view.Close())
}

// Lookup queries the live catalog in a freshly validated snapshot.
func (store *Store) Lookup(query string, options catalog.Options) ([]catalog.Result, error) {
	view, err := store.openReadView()
	if err != nil {
		return nil, err
	}
	results, readErr := view.Lookup(query, options)
	return results, readResultError(readErr, view.Close())
}

// CurrentVersion returns the current version from a freshly validated snapshot.
func (store *Store) CurrentVersion(reqPath string) (int, error) {
	view, err := store.openReadView()
	if err != nil {
		return 0, err
	}
	version, readErr := view.currentVersion(reqPath)
	return version, readResultError(readErr, view.Close())
}

func readResultError(readErr, closeErr error) error {
	if readErr == nil {
		return closeErr
	}
	if closeErr == nil {
		return readErr
	}
	return errors.Join(readErr, closeErr)
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
	archived bool
}

func (view *readView) Get(reqPath string, version int) (*protocolstore.Document, error) {
	entry, err := view.documentEntry(reqPath)
	if err != nil {
		return nil, err
	}
	if version < 0 {
		return nil, os.ErrNotExist
	}
	history, err := view.loadHistory(&entry)
	if err != nil {
		return nil, err
	}
	requested := version
	if requested == 0 {
		requested = history.manifest.Current
	}
	retained, ok := retainedAt(history.versions, requested)
	if !ok {
		return nil, os.ErrNotExist
	}
	raw, err := view.loadBlob(retained.entry.Blob)
	if err != nil {
		return nil, fmt.Errorf("load v%d: %w", retained.entry.Version, err)
	}
	stored, err := validateStoredDocument(raw, &retained, &entry, requested == history.manifest.Current)
	if err != nil {
		return nil, err
	}
	if err := view.ctx.Err(); err != nil {
		return nil, err
	}
	return &protocolstore.Document{
		Content:  bytes.Clone(stored.body),
		Modified: retained.modified,
		Version:  retained.entry.Version,
		Archived: stored.archived,
		Metadata: maps.Clone(stored.metadata),
		ETag:     protocolstore.StoredETag(raw),
	}, nil
}

func (view *readView) ListEntries(reqPath string, includeArchived bool) ([]protocolstore.DirEntry, error) {
	logicalPath, err := view.logicalPath(reqPath)
	if err != nil {
		return nil, err
	}
	if _, isDocument := view.snapshot.Paths[logicalPath]; isDocument {
		return nil, os.ErrNotExist
	}
	directory, exists := view.snapshot.Directories[logicalPath]
	if !exists {
		return nil, os.ErrNotExist
	}
	entries := make([]protocolstore.DirEntry, 0, len(directory.Children))
	for _, child := range directory.Children {
		if hiddenLogicalName(child.Name) || (includeArchived && !child.Visible) || (!includeArchived && !child.Live) {
			continue
		}
		entries = append(entries, protocolstore.DirEntry{Name: child.Name, IsDir: child.IsDir})
	}
	if err := view.ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (view *readView) IsDir(reqPath string) (bool, error) {
	logicalPath, err := view.logicalPath(reqPath)
	if err != nil {
		return false, err
	}
	if _, exists := view.snapshot.Directories[logicalPath]; exists {
		return true, nil
	}
	if _, exists := view.snapshot.Paths[logicalPath]; exists {
		return false, nil
	}
	return false, os.ErrNotExist
}

func (view *readView) Versions(reqPath string) ([]protocolstore.VersionInfo, error) {
	entry, err := view.documentEntry(reqPath)
	if err != nil {
		return nil, err
	}
	history, err := view.loadHistory(&entry)
	if err != nil {
		return nil, err
	}
	versions := make([]protocolstore.VersionInfo, 0, len(history.versions))
	for _, retained := range slices.Backward(history.versions) {
		versions = append(versions, protocolstore.VersionInfo{
			Version:  retained.entry.Version,
			Modified: retained.modified,
		})
	}
	if err := view.ctx.Err(); err != nil {
		return nil, err
	}
	return versions, nil
}

func (view *readView) LookupHash(hash string) (string, error) {
	if err := view.ctx.Err(); err != nil {
		return "", err
	}
	path, exists := view.snapshot.BodyHashes[hash]
	if !exists {
		return "", os.ErrNotExist
	}
	return path, nil
}

func (view *readView) VerifyChain(reqPath string) error {
	err := view.verifyChain(reqPath)
	if errors.Is(err, blob.ErrIntegrity) {
		return errors.Join(protocolstore.ErrIntegrity, err)
	}
	return err
}

func (view *readView) verifyChain(reqPath string) error {
	entry, err := view.documentEntry(reqPath)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	history, err := view.loadHistory(&entry)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	previous := history.versions[0]
	previousRaw, err := view.loadBlobUnchecked(previous.entry.Blob)
	if err != nil {
		return fmt.Errorf("read v%d: %w", previous.entry.Version, err)
	}
	for _, current := range history.versions[1:] {
		currentRaw, err := view.loadBlobUnchecked(current.entry.Blob)
		if err != nil {
			return fmt.Errorf("read v%d: %w", current.entry.Version, err)
		}
		recorded := protocolstore.ExtractPreviousHash(currentRaw)
		if recorded == "" {
			return fmt.Errorf("%w: v%d missing previous-hash", blob.ErrIntegrity, current.entry.Version)
		}
		expected := "sha256-" + protocolstore.StoredETag(previousRaw)
		if recorded != expected {
			return fmt.Errorf("%w: v%d chain broken: previous-hash mismatch (want %s, got %s)",
				blob.ErrIntegrity, current.entry.Version, expected, recorded)
		}
		if _, err := validateStoredDocument(previousRaw, &previous, &entry, false); err != nil {
			return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
		}
		if err := verifyBlobHash(previous.entry.Blob, previousRaw); err != nil {
			return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
		}
		previous = current
		previousRaw = currentRaw
	}
	if _, err := validateStoredDocument(previousRaw, &previous, &entry, true); err != nil {
		return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
	}
	if err := verifyBlobHash(previous.entry.Blob, previousRaw); err != nil {
		return fmt.Errorf("validate v%d: %w", previous.entry.Version, err)
	}
	return view.ctx.Err()
}

func (view *readView) Lookup(query string, options catalog.Options) ([]catalog.Result, error) {
	if err := view.ctx.Err(); err != nil {
		return nil, err
	}
	results, err := view.snapshot.Catalog.Lookup(query, options)
	if err != nil {
		return nil, err
	}
	if err := view.ctx.Err(); err != nil {
		return nil, err
	}
	cloned := slices.Clone(results)
	for index := range results {
		cloned[index].Tags = slices.Clone(results[index].Tags)
		cloned[index].Metadata = maps.Clone(results[index].Metadata)
	}
	if err := view.ctx.Err(); err != nil {
		return nil, err
	}
	return cloned, nil
}

func (view *readView) currentVersion(reqPath string) (int, error) {
	logicalPath, err := view.logicalPath(reqPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	entry, exists := view.snapshot.Paths[logicalPath]
	if !exists {
		return 0, nil
	}
	return entry.Current, nil
}

func (view *readView) documentEntry(reqPath string) (snapshotEntry, error) {
	logicalPath, err := view.logicalPath(reqPath)
	if err != nil {
		return snapshotEntry{}, err
	}
	entry, exists := view.snapshot.Paths[logicalPath]
	if !exists {
		return snapshotEntry{}, os.ErrNotExist
	}
	return entry, nil
}

func (view *readView) logicalPath(reqPath string) (string, error) {
	if err := view.ctx.Err(); err != nil {
		return "", err
	}
	relative, err := protocolstore.RelPath(reqPath)
	if err != nil {
		return "", err
	}
	if relative == "" {
		return "/", nil
	}
	return "/" + relative, nil
}

func (view *readView) loadHistory(entry *snapshotEntry) (retainedHistory, error) {
	manifest, err := getImmutable(view.ctx, view.objects, entry.Manifest, manifestKey(entry.PathHash, entry.Manifest.Hash), validateManifestObject)
	if err != nil {
		return retainedHistory{}, fmt.Errorf("load manifest: %w", err)
	}
	if manifest.PathHash != entry.PathHash || manifest.Current != entry.Current || manifest.Archived != entry.Archived {
		return retainedHistory{}, fmt.Errorf("%w: manifest does not match shard entry", blob.ErrIntegrity)
	}

	loaded := retainedHistory{manifest: manifest}
	for index, ref := range manifest.History {
		history, err := getImmutable(view.ctx, view.objects, ref.objectRef, historyKey(ref.Hash), func(history *historyObject) error {
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

func (view *readView) loadBlob(ref objectRef) ([]byte, error) {
	data, err := view.loadBlobUnchecked(ref)
	if err != nil {
		return nil, err
	}
	if err := verifyBlobHash(ref, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (view *readView) loadBlobUnchecked(ref objectRef) ([]byte, error) {
	if err := verifyRef(ref, blobKey(ref.Hash)); err != nil {
		return nil, fmt.Errorf("%w: %v", blob.ErrIntegrity, err)
	}
	value, err := view.objects.Get(view.ctx, ref.Key)
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

func validateStoredDocument(raw []byte, retained *retainedVersion, entry *snapshotEntry, tip bool) (storedDocument, error) {
	header, err := protocolstore.InspectStoredVersion(raw)
	if err != nil {
		return storedDocument{}, fmt.Errorf("%w: v%d stored version: %v", blob.ErrIntegrity, retained.entry.Version, err)
	}
	version := header.Version
	if version != retained.entry.Version {
		return storedDocument{}, fmt.Errorf("%w: stored version is %d, history says %d", blob.ErrIntegrity, version, retained.entry.Version)
	}
	body := protocolstore.ExtractBody(raw)
	if bodyHash := protocolstore.ContentHash(body); bodyHash != retained.entry.BodyHash {
		return storedDocument{}, fmt.Errorf("%w: v%d body hash is %s, history says %s", blob.ErrIntegrity, version, bodyHash, retained.entry.BodyHash)
	}
	metadata := protocolstore.ExtractMetadata(raw)
	if err := protocolstore.ValidateWrite(body, metadata); err != nil {
		return storedDocument{}, fmt.Errorf("%w: v%d stored data: %v", blob.ErrIntegrity, version, err)
	}
	archived := header.Archived
	if tip && archived != entry.Archived {
		return storedDocument{}, fmt.Errorf("%w: v%d archive state does not match manifest", blob.ErrIntegrity, version)
	}
	if !tip && archived {
		return storedDocument{}, fmt.Errorf("%w: non-tip v%d is archived", blob.ErrIntegrity, version)
	}
	return storedDocument{body: body, metadata: metadata, archived: archived}, nil
}
