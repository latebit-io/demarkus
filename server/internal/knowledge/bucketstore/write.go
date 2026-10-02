package bucketstore

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	pathpkg "path"
	"slices"
	"strconv"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// candidateMutation is one change ready to commit: its slot entry, the
// content-addressed objects staged before the slot, and the body this
// replica's section index takes (nil when the document is archived).
type candidateMutation struct {
	entry   slotEntry
	objects []modelObject
	body    []byte
}

// mutationResult is a committed or refused mutation.
type mutationResult struct {
	Document *storefmt.Document
	Changed  bool
	Sequence int64 // log sequence of the commit, once it succeeded
}

type mutationBuilder func(context.Context, *readView, string) (*candidateMutation, mutationResult, error)

// Publish commits one version.
func (store *Store) Publish(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	result, err := store.publish(ctx, req)
	return result.Document, err
}

// publish keeps the policy output that Publish drops.
func (store *Store) publish(ctx context.Context, req backend.WriteRequest) (mutationResult, error) {
	path, expected, content, metadata := req.Path, req.ExpectedVersion, req.Content, req.Metadata
	if err := storefmt.ValidateWrite(content, metadata); err != nil {
		return mutationResult{}, err
	}
	canonical, err := canonicalMutationPath(path)
	if err != nil {
		return mutationResult{}, err
	}
	body := bytes.Clone(content)
	meta := maps.Clone(metadata)
	blindBase := -1
	return store.runMutation(ctx, canonical, func(ctx context.Context, view *readView, operationID string) (*candidateMutation, mutationResult, error) {
		if view.path(canonical) == nil {
			if err := store.checkDocumentQuota(view); err != nil {
				return nil, mutationResult{}, err
			}
		}
		if expected < 0 {
			current := 0
			if entry := view.path(canonical); entry != nil {
				current = entry.Current
			}
			if blindBase < 0 {
				blindBase = current
			} else if current != blindBase {
				return nil, conflictResult(current), storefmt.ErrConflict
			}
		}
		write := writeCandidate{path: canonical, op: protocol.OpPublish, expected: expected, body: body, metadata: meta, precondition: req.Precondition}
		return store.buildWriteCandidate(ctx, view, operationID, &write)
	})
}

// ErrDocumentQuota rejects a write that would create a new document
// path beyond Options.MaxDocuments. Existing documents keep accepting
// updates, appends, and archives.
var ErrDocumentQuota = fmt.Errorf("document %w", backend.ErrQuota)

// checkDocumentQuota gates new-path creation against MaxDocuments.
func (store *Store) checkDocumentQuota(view *readView) error {
	if store.maxDocuments <= 0 {
		return nil
	}
	if count := view.snapshot.Paths.Len(); count >= store.maxDocuments {
		return fmt.Errorf("%w: world holds %d documents (limit %d)", ErrDocumentQuota, count, store.maxDocuments)
	}
	return nil
}

// Append commits content on the exact expected version.
func (store *Store) Append(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	result, err := store.appendVersion(ctx, req)
	return result.Document, err
}

// appendVersion keeps the policy output that Append drops.
func (store *Store) appendVersion(ctx context.Context, req backend.WriteRequest) (mutationResult, error) {
	path, expected, content, metadata := req.Path, req.ExpectedVersion, req.Content, req.Metadata
	if expected < 1 {
		return mutationResult{}, fmt.Errorf("APPEND requires expected-version >= 1, got %d", expected)
	}
	if len(content) == 0 {
		return mutationResult{}, fmt.Errorf("APPEND requires non-empty content")
	}
	if err := storefmt.ValidateMeta(metadata); err != nil {
		return mutationResult{}, err
	}
	canonical, err := canonicalMutationPath(path)
	if err != nil {
		return mutationResult{}, err
	}
	addition := bytes.Clone(content)
	meta := maps.Clone(metadata)
	return store.runMutation(ctx, canonical, func(ctx context.Context, view *readView, operationID string) (*candidateMutation, mutationResult, error) {
		entry := view.path(canonical)
		if entry == nil {
			return nil, mutationResult{}, backend.ErrNotFound
		}
		if entry.Current != expected {
			return nil, conflictResult(entry.Current), storefmt.ErrConflict
		}
		base, err := view.Get(ctx, canonical, expected)
		if err != nil {
			return nil, mutationResult{}, err
		}
		combined, err := storefmt.JoinContent(base.Content, addition)
		if err != nil {
			return nil, mutationResult{}, err
		}
		merged := storefmt.PrepareAppendMeta(canonical, base.Metadata, meta)
		if err := storefmt.ValidateWrite(combined, merged); err != nil {
			return nil, mutationResult{}, err
		}
		write := writeCandidate{path: canonical, op: protocol.OpAppend, expected: expected, body: combined, metadata: merged, precondition: req.Precondition}
		return store.buildWriteCandidate(ctx, view, operationID, &write)
	})
}

// SetArchived atomically toggles operational archive state.
func (store *Store) SetArchived(ctx context.Context, req backend.ArchiveRequest) (backend.ArchiveResult, error) {
	canonical, err := canonicalMutationPath(req.Path)
	if err != nil {
		return backend.ArchiveResult{}, err
	}
	archive := archiveCandidate{
		change:       storefmt.ArchiveChange{Path: canonical, Archived: req.Archived},
		precondition: req.Precondition,
	}
	result, err := store.runMutation(ctx, canonical, func(ctx context.Context, view *readView, operationID string) (*candidateMutation, mutationResult, error) {
		return store.buildArchiveCandidate(ctx, view, operationID, &archive)
	})
	return backend.ArchiveResult{Document: result.Document, Changed: result.Changed}, err
}

func canonicalMutationPath(path string) (string, error) {
	relative, err := storefmt.RelPath(path)
	// Invalid paths stay not-found to avoid exposing storage topology.
	if err != nil || relative == "" {
		return "", backend.ErrNotFound
	}
	canonical := "/" + relative
	if err := protocol.ValidateRequestPath(canonical); err != nil {
		return "", backend.ErrNotFound
	}
	return canonical, nil
}

func conflictResult(version int) mutationResult {
	return mutationResult{Document: &storefmt.Document{Version: version}}
}

// writeCandidate is one prepared PUBLISH or APPEND inside a commit attempt.
type writeCandidate struct {
	path, op     string
	expected     int
	body         []byte
	metadata     map[string]string
	precondition backend.Precondition
}

// checkWritable refuses what no version check can fix: an archived document,
// a new path that collides with the topology, a spent version range.
func checkWritable(loaded snapshotReader, path string) error {
	entry := loaded.path(path)
	switch {
	case entry == nil:
		return validateNewPathTopology(loaded, path)
	case entry.Archived:
		return storefmt.ErrArchived
	case entry.Current >= storefmt.MaxVersionNumber:
		return fmt.Errorf("version limit %d reached", storefmt.MaxVersionNumber)
	}
	return nil
}

// writeBase is what a write to an existing document builds on. unchanged is
// set when the write would repeat the tip, body and metadata both.
type writeBase struct {
	history     []retainedVersion
	previousRaw []byte
	unchanged   *storefmt.Document
}

func loadWriteBase(ctx context.Context, view *readView, entry *pathState, write *writeCandidate) (writeBase, error) {
	history, err := view.history(ctx, entry)
	if err != nil {
		return writeBase{}, err
	}
	tip := history[len(history)-1]
	previousRaw, storedTip, err := view.loadStored(ctx, &tip)
	if err != nil {
		return writeBase{}, err
	}
	base := writeBase{history: history, previousRaw: previousRaw}
	if bytes.Equal(storedTip.body, write.body) && storefmt.MetaEqual(storedTip.metadata, storefmt.NormalizeMetadata(write.metadata)) {
		base.unchanged = documentFromRetained(previousRaw, &tip, &storedTip, false)
	}
	return base, nil
}

func (store *Store) buildWriteCandidate(
	ctx context.Context,
	view *readView,
	operationID string,
	write *writeCandidate,
) (*candidateMutation, mutationResult, error) {
	path, expected, body, metadata := write.path, write.expected, write.body, write.metadata
	entry := view.path(path)
	current := 0
	if entry != nil {
		current = entry.Current
	}
	if expected >= 0 && current != expected {
		return nil, conflictResult(current), storefmt.ErrConflict
	}
	if err := checkWritable(view, path); err != nil {
		return nil, mutationResult{}, err
	}

	var base writeBase
	if entry != nil {
		var err error
		if base, err = loadWriteBase(ctx, view, entry, write); err != nil {
			return nil, mutationResult{}, err
		}
		if base.unchanged != nil {
			return nil, mutationResult{Document: base.unchanged}, storefmt.ErrNotModified
		}
	}

	next := current + 1
	stored, err := storefmt.SerializeVersion(next, base.previousRaw, body, metadata)
	if err != nil {
		return nil, mutationResult{}, err
	}
	modified := store.now().UTC().Truncate(time.Second)
	persisted := storefmt.ExtractMetadata(stored)
	if write.precondition != nil {
		// Each commit attempt judges the write against the snapshot it rebased on.
		prepared := storefmt.PreparedWrite{Path: path, Content: body, Metadata: persisted}
		if err := write.precondition(ctx, attemptView(ctx, view), prepared); err != nil {
			return nil, mutationResult{}, err
		}
	}

	blobHash := hashHex(stored)
	versionEntry := historyEntry{
		Version:  next,
		Blob:     objectRef{Key: blobKey(blobHash), Hash: blobHash},
		BodyHash: storefmt.ContentHash(body),
		Modified: modified.Format(time.RFC3339),
	}
	versions := append(slices.Clone(base.history), retainedVersion{entry: versionEntry, modified: modified})
	prune := applyRetention(&versions, metadata)
	record := makeCatalogRecord(path, persisted, body, modified)
	document := &storefmt.Document{
		Content:  bytes.Clone(body),
		Modified: modified,
		Version:  next,
		Archived: false,
		Metadata: maps.Clone(persisted),
		ETag:     blobHash,
		Prune:    prune,
	}
	return &candidateMutation{
		entry: slotEntry{
			OperationID: operationID, Op: write.op, Agent: persisted["agent"], Path: path,
			Current: next, First: versions[0].entry.Version, BodyHash: versionEntry.BodyHash,
			Modified: versionEntry.Modified, Catalog: &record, Version: &versionEntry,
		},
		objects: []modelObject{{Key: versionEntry.Blob.Key, Data: stored}},
		body:    body,
	}, mutationResult{Document: document, Changed: true}, nil
}

// archiveCandidate is one canonical archive transition inside a commit attempt.
type archiveCandidate struct {
	change       storefmt.ArchiveChange
	precondition backend.ArchivePrecondition
}

func (store *Store) buildArchiveCandidate(
	ctx context.Context,
	view *readView,
	operationID string,
	archive *archiveCandidate,
) (*candidateMutation, mutationResult, error) {
	path, archived := archive.change.Path, archive.change.Archived
	entry := view.path(path)
	if entry == nil {
		return nil, mutationResult{}, backend.ErrNotFound
	}
	history, err := view.history(ctx, entry)
	if err != nil {
		return nil, mutationResult{}, err
	}
	tip := history[len(history)-1]
	raw, storedTip, err := view.loadStored(ctx, &tip)
	if err != nil {
		return nil, mutationResult{}, err
	}
	document := documentFromRetained(raw, &tip, &storedTip, archived)
	if entry.Archived == archived {
		return nil, mutationResult{Document: document, Changed: false}, nil
	}
	if archive.precondition != nil {
		// Judged again on every rebased attempt, as a write is.
		if err := archive.precondition(ctx, attemptView(ctx, view), archive.change); err != nil {
			return nil, mutationResult{}, err
		}
	}
	var body []byte
	if !archived {
		body = storedTip.body
	}
	return &candidateMutation{
		entry: slotEntry{
			OperationID: operationID, Op: changefeed.ArchiveOp(archived), Agent: document.Metadata["agent"],
			Path: path, Current: entry.Current, First: history[0].entry.Version, Archived: archived,
			BodyHash: entry.BodyHash, Modified: entry.Modified.Format(time.RFC3339),
		},
		body: body,
	}, mutationResult{Document: document, Changed: true}, nil
}

// snapshotReader reads a snapshot directly when replaying, or through a commit
// attempt's view, which records what it read.
type snapshotReader interface {
	path(path string) *pathState
	isDirectory(path string) bool
}

func validateNewPathTopology(snapshot snapshotReader, path string) error {
	if snapshot.isDirectory(path) {
		return fmt.Errorf("cannot publish %s: a directory exists at this path: %w", path, storefmt.ErrPathCollision)
	}
	for ancestor := pathpkg.Dir(path); ancestor != "/"; ancestor = pathpkg.Dir(ancestor) {
		if snapshot.path(ancestor) != nil {
			return fmt.Errorf("cannot publish %s: a document exists at ancestor %s: %w", path, ancestor, storefmt.ErrPathCollision)
		}
	}
	return nil
}

func applyRetention(versions *[]retainedVersion, metadata map[string]string) *storefmt.PruneResult {
	keep := storefmt.RetentionValue(metadata)
	if keep <= 0 || len(*versions) <= keep {
		return nil
	}
	remove := len(*versions) - keep
	prune := &storefmt.PruneResult{
		From: (*versions)[0].entry.Version,
		To:   (*versions)[remove-1].entry.Version,
	}
	*versions = slices.Clone((*versions)[remove:])
	return prune
}

func makeCatalogRecord(path string, metadata map[string]string, body []byte, modified time.Time) catalogRecord {
	entry := catalog.FromDocument(path, metadata, body, modified)
	tags := slices.Clone(entry.Tags)
	if tags == nil {
		tags = make([]string, 0)
	}
	storedMetadata := maps.Clone(entry.Metadata)
	if storedMetadata == nil {
		storedMetadata = make(map[string]string)
	}
	return catalogRecord{
		Path:       path,
		Title:      entry.Title,
		Tags:       tags,
		Importance: strconv.FormatFloat(entry.Importance, 'f', -1, 64),
		Modified:   modified.Format(time.RFC3339),
		Metadata:   storedMetadata,
	}
}

// catalogRecordOf is the record a prepared catalog entry was built from.
func catalogRecordOf(entry *catalog.Entry) catalogRecord {
	tags := entry.Tags
	if tags == nil {
		tags = make([]string, 0)
	}
	metadata := entry.Metadata
	if metadata == nil {
		metadata = make(map[string]string)
	}
	return catalogRecord{
		Path:       entry.Path,
		Title:      entry.Title,
		Tags:       tags,
		Importance: strconv.FormatFloat(entry.Importance, 'f', -1, 64),
		Modified:   entry.Modified.UTC().Format(time.RFC3339),
		Metadata:   metadata,
	}
}

func documentFromRetained(raw []byte, retained *retainedVersion, stored *storedDocument, archived bool) *storefmt.Document {
	return &storefmt.Document{
		Content:  bytes.Clone(stored.body),
		Modified: retained.modified,
		Version:  retained.entry.Version,
		Archived: archived,
		Metadata: maps.Clone(stored.metadata),
		ETag:     storefmt.StoredETag(raw),
	}
}
