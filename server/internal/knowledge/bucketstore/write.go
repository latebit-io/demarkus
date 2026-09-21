package bucketstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

type candidateMutation struct {
	base        *snapshot
	head        headObject
	headData    []byte
	root        rootObject
	shard       shardObject
	shardIndex  int
	objects     []modelObject
	result      MutationResult
	operationID string
	prepared    *snapshot
}

// MutationResult is a committed or refused mutation.
type MutationResult struct {
	Document *storefmt.Document
	Changed  bool
}

type mutationBuilder func(context.Context, *readView, string) (*candidateMutation, MutationResult, error)

// Publish commits one version.
func (store *Store) Publish(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	result, err := store.publish(ctx, req)
	return result.Document, err
}

// publish keeps the policy output that Publish drops.
func (store *Store) publish(ctx context.Context, req backend.WriteRequest) (MutationResult, error) {
	path, expected, content, metadata := req.Path, req.ExpectedVersion, req.Content, req.Metadata
	if err := storefmt.ValidateWrite(content, metadata); err != nil {
		return MutationResult{}, err
	}
	canonical, err := canonicalMutationPath(path)
	if err != nil {
		return MutationResult{}, err
	}
	body := bytes.Clone(content)
	meta := maps.Clone(metadata)
	blindBase := -1
	return store.runMutation(ctx, func(ctx context.Context, view *readView, operationID string) (*candidateMutation, MutationResult, error) {
		if _, exists := view.snapshot.Paths[canonical]; !exists {
			if err := store.checkDocumentQuota(view); err != nil {
				return nil, MutationResult{}, err
			}
		}
		if expected < 0 {
			current := 0
			if entry, exists := view.snapshot.Paths[canonical]; exists {
				current = entry.Current
			}
			if blindBase < 0 {
				blindBase = current
			} else if current != blindBase {
				return nil, conflictResult(current), storefmt.ErrConflict
			}
		}
		write := writeCandidate{path: canonical, expected: expected, body: body, metadata: meta, precondition: req.Precondition}
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
	if len(view.snapshot.Paths) >= store.maxDocuments {
		return fmt.Errorf("%w: world holds %d documents (limit %d)", ErrDocumentQuota, len(view.snapshot.Paths), store.maxDocuments)
	}
	return nil
}

// Append commits content on the exact expected version.
func (store *Store) Append(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	result, err := store.appendVersion(ctx, req)
	return result.Document, err
}

// appendVersion keeps the policy output that Append drops.
func (store *Store) appendVersion(ctx context.Context, req backend.WriteRequest) (MutationResult, error) {
	path, expected, content, metadata := req.Path, req.ExpectedVersion, req.Content, req.Metadata
	if expected < 1 {
		return MutationResult{}, fmt.Errorf("APPEND requires expected-version >= 1, got %d", expected)
	}
	if len(content) == 0 {
		return MutationResult{}, fmt.Errorf("APPEND requires non-empty content")
	}
	if err := storefmt.ValidateMeta(metadata); err != nil {
		return MutationResult{}, err
	}
	canonical, err := canonicalMutationPath(path)
	if err != nil {
		return MutationResult{}, err
	}
	addition := bytes.Clone(content)
	meta := maps.Clone(metadata)
	return store.runMutation(ctx, func(ctx context.Context, view *readView, operationID string) (*candidateMutation, MutationResult, error) {
		entry, exists := view.snapshot.Paths[canonical]
		if !exists {
			return nil, MutationResult{}, backend.ErrNotFound
		}
		if entry.Current != expected {
			return nil, conflictResult(entry.Current), storefmt.ErrConflict
		}
		base, err := view.Get(canonical, expected)
		if err != nil {
			return nil, MutationResult{}, err
		}
		combined, err := storefmt.JoinContent(base.Content, addition)
		if err != nil {
			return nil, MutationResult{}, err
		}
		merged := storefmt.PrepareAppendMeta(canonical, base.Metadata, meta)
		if err := storefmt.ValidateWrite(combined, merged); err != nil {
			return nil, MutationResult{}, err
		}
		write := writeCandidate{path: canonical, expected: expected, body: combined, metadata: merged, precondition: req.Precondition}
		return store.buildWriteCandidate(ctx, view, operationID, &write)
	})
}

// SetArchived atomically toggles operational archive state.
func (store *Store) SetArchived(ctx context.Context, path string, archived bool) (backend.ArchiveResult, error) {
	canonical, err := canonicalMutationPath(path)
	if err != nil {
		return backend.ArchiveResult{}, err
	}
	result, err := store.runMutation(ctx, func(_ context.Context, view *readView, operationID string) (*candidateMutation, MutationResult, error) {
		return store.buildArchiveCandidate(view, operationID, canonical, archived)
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

func conflictResult(version int) MutationResult {
	return MutationResult{Document: &storefmt.Document{Version: version}}
}

// writeCandidate is one prepared PUBLISH or APPEND inside a commit attempt.
type writeCandidate struct {
	path         string
	expected     int
	body         []byte
	metadata     map[string]string
	precondition backend.Precondition
}

// checkWritable refuses what no version check can fix: an archived document,
// a new path that collides with the topology, a spent version range.
func checkWritable(loaded *snapshot, path string) error {
	entry, exists := loaded.Paths[path]
	switch {
	case !exists:
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
	history     retainedHistory
	previousRaw []byte
	unchanged   *storefmt.Document
}

func loadWriteBase(view *readView, entry *snapshotEntry, write *writeCandidate) (writeBase, error) {
	history, err := view.loadHistory(entry)
	if err != nil {
		return writeBase{}, err
	}
	tip := history.versions[len(history.versions)-1]
	previousRaw, err := view.loadBlob(tip.entry.Blob)
	if err != nil {
		return writeBase{}, err
	}
	storedTip, err := validateStoredDocument(previousRaw, &tip)
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
) (*candidateMutation, MutationResult, error) {
	path, expected, body, metadata := write.path, write.expected, write.body, write.metadata
	entry, exists := view.snapshot.Paths[path]
	current := 0
	if exists {
		current = entry.Current
	}
	if expected >= 0 && current != expected {
		return nil, conflictResult(current), storefmt.ErrConflict
	}
	if err := checkWritable(view.snapshot, path); err != nil {
		return nil, MutationResult{}, err
	}

	var base writeBase
	if exists {
		var err error
		if base, err = loadWriteBase(view, &entry, write); err != nil {
			return nil, MutationResult{}, err
		}
		if base.unchanged != nil {
			return nil, MutationResult{Document: base.unchanged}, storefmt.ErrNotModified
		}
	}
	history, previousRaw := base.history, base.previousRaw

	next := current + 1
	stored, err := storefmt.SerializeVersion(next, previousRaw, body, metadata)
	if err != nil {
		return nil, MutationResult{}, err
	}
	modified := store.now().UTC().Truncate(time.Second)
	persisted := storefmt.ExtractMetadata(stored)
	if write.precondition != nil {
		// Each commit attempt judges the write against the snapshot it rebased on.
		prepared := storefmt.PreparedWrite{Path: path, Content: body, Metadata: persisted}
		if err := write.precondition(ctx, attemptView(view), prepared); err != nil {
			return nil, MutationResult{}, err
		}
	}

	blobHash := hashHex(stored)
	versionEntry := historyEntry{
		Version:  next,
		Blob:     objectRef{Key: blobKey(blobHash), Hash: blobHash},
		BodyHash: storefmt.ContentHash(body),
		Modified: modified.Format(time.RFC3339),
	}
	versions := append(slices.Clone(history.versions), retainedVersion{entry: versionEntry, modified: modified})
	prune := applyRetention(&versions, metadata)
	oldManifest := history.manifest
	if !exists {
		oldManifest = manifestObject{History: make([]historyRef, 0)}
	}
	historyRefs, historyObjects, err := buildHistoryBlocks(pathHash(path), versions, oldManifest.History, map[int]bool{next: true})
	if err != nil {
		return nil, MutationResult{}, err
	}
	manifest := manifestObject{
		Schema:   schemaVersion,
		PathHash: pathHash(path),
		Current:  next,
		Archived: false,
		History:  historyRefs,
	}
	manifestModel, manifestRef, err := buildManifestModel(manifest)
	if err != nil {
		return nil, MutationResult{}, err
	}
	catalogRecord := makeCatalogRecord(path, persisted, body, modified)
	newEntry := shardEntry{
		Path:     path,
		PathHash: pathHash(path),
		Manifest: manifestRef,
		Current:  next,
		Archived: false,
		BodyHash: versionEntry.BodyHash,
		Modified: versionEntry.Modified,
		Catalog:  catalogRecord,
	}
	document := &storefmt.Document{
		Content:  bytes.Clone(body),
		Modified: modified,
		Version:  next,
		Archived: false,
		Metadata: maps.Clone(persisted),
		ETag:     blobHash,
		Prune:    prune,
	}
	objects := make([]modelObject, 0, len(historyObjects)+4)
	objects = append(objects, modelObject{Key: versionEntry.Blob.Key, Data: bytes.Clone(stored)})
	objects = append(objects, historyObjects...)
	objects = append(objects, manifestModel)
	result := MutationResult{Document: document, Changed: true}
	return store.buildNamespaceCandidate(view.ctx, view.snapshot, operationID, &newEntry, !exists, objects, result, body)
}

func (store *Store) buildArchiveCandidate(
	view *readView,
	operationID, path string,
	archived bool,
) (*candidateMutation, MutationResult, error) {
	entry, exists := view.snapshot.Paths[path]
	if !exists {
		return nil, MutationResult{}, backend.ErrNotFound
	}
	history, err := view.loadHistory(&entry)
	if err != nil {
		return nil, MutationResult{}, err
	}
	tip := history.versions[len(history.versions)-1]
	raw, err := view.loadBlob(tip.entry.Blob)
	if err != nil {
		return nil, MutationResult{}, err
	}
	storedTip, err := validateStoredDocument(raw, &tip)
	if err != nil {
		return nil, MutationResult{}, err
	}
	if entry.Archived == archived {
		document := documentFromRetained(raw, &tip, &storedTip, archived)
		return nil, MutationResult{Document: document, Changed: false}, nil
	}
	manifest := history.manifest
	manifest.Archived = archived
	manifestModel, manifestRef, err := buildManifestModel(manifest)
	if err != nil {
		return nil, MutationResult{}, err
	}
	oldEntry, err := snapshotShardEntry(view.snapshot, path)
	if err != nil {
		return nil, MutationResult{}, err
	}
	oldEntry.Manifest = manifestRef
	oldEntry.Archived = archived
	objects := []modelObject{manifestModel}
	document := documentFromRetained(raw, &tip, &storedTip, archived)
	result := MutationResult{Document: document, Changed: true}
	return store.buildNamespaceCandidate(view.ctx, view.snapshot, operationID, &oldEntry, false, objects, result, nil)
}

func validateNewPathTopology(snapshot *snapshot, path string) error {
	if _, exists := snapshot.Directories[path]; exists {
		return fmt.Errorf("cannot publish %s: a directory exists at this path: %w", path, storefmt.ErrPathCollision)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	ancestor := ""
	for _, part := range parts[:len(parts)-1] {
		ancestor += "/" + part
		if _, exists := snapshot.Paths[ancestor]; exists {
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

func buildHistoryBlocks(
	documentPathHash string,
	versions []retainedVersion,
	oldRefs []historyRef,
	dirty map[int]bool,
) ([]historyRef, []modelObject, error) {
	oldByRange := make(map[[2]int]historyRef, len(oldRefs))
	for _, ref := range oldRefs {
		oldByRange[[2]int{ref.First, ref.Last}] = ref
	}
	refs := make([]historyRef, 0, len(versions)/historyBlockSize+1)
	objects := make([]modelObject, 0, cap(refs))
	for start := 0; start < len(versions); {
		block := (versions[start].entry.Version - 1) / historyBlockSize
		end := start + 1
		for end < len(versions) && (versions[end].entry.Version-1)/historyBlockSize == block {
			end++
		}
		first := versions[start].entry.Version
		last := versions[end-1].entry.Version
		reuse := true
		for index := start; index < end; index++ {
			if dirty[versions[index].entry.Version] {
				reuse = false
				break
			}
		}
		if old, exists := oldByRange[[2]int{first, last}]; exists && reuse {
			refs = append(refs, old)
			start = end
			continue
		}
		entries := make([]historyEntry, end-start)
		for index := range entries {
			entries[index] = versions[start+index].entry
		}
		history := historyObject{
			Schema:   schemaVersion,
			PathHash: documentPathHash,
			First:    first,
			Last:     last,
			Entries:  entries,
		}
		if err := validateHistoryObject(&history); err != nil {
			return nil, nil, fmt.Errorf("build history %d-%d: %w", first, last, err)
		}
		model, ref, err := immutableJSON(historyKey, history)
		if err != nil {
			return nil, nil, fmt.Errorf("build history %d-%d: %w", first, last, err)
		}
		refs = append(refs, historyRef{
			PathHash:  documentPathHash,
			First:     first,
			Last:      last,
			objectRef: ref,
		})
		objects = append(objects, model)
		start = end
	}
	return refs, objects, nil
}

func buildManifestModel(manifest manifestObject) (modelObject, objectRef, error) {
	if err := validateManifestObject(&manifest); err != nil {
		return modelObject{}, objectRef{}, fmt.Errorf("build manifest: %w", err)
	}
	model, ref, err := immutableJSON(func(hash string) string {
		return manifestKey(manifest.PathHash, hash)
	}, manifest)
	if err != nil {
		return modelObject{}, objectRef{}, fmt.Errorf("build manifest: %w", err)
	}
	return model, ref, nil
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

func snapshotShardEntry(snapshot *snapshot, path string) (shardEntry, error) {
	index := int(pathHashBytes(path)[0])
	entries := snapshot.Shards[index].Entries
	position := sort.Search(len(entries), func(position int) bool { return entries[position].Path >= path })
	if position == len(entries) || entries[position].Path != path {
		return shardEntry{}, fmt.Errorf("%w: snapshot path %s has no shard entry", storefmt.ErrIntegrity, path)
	}
	return entries[position], nil
}

// buildNamespaceCandidate derives the committed snapshot from base plus one
// changed entry; body is the written content, nil for an archive flip, so
// the section index needs at most one bucket read.
func (store *Store) buildNamespaceCandidate(
	ctx context.Context,
	base *snapshot,
	operationID string,
	entry *shardEntry,
	created bool,
	objects []modelObject,
	result MutationResult,
	body []byte,
) (*candidateMutation, MutationResult, error) {
	shardIndex := int(pathHashBytes(entry.Path)[0])
	shard := base.Shards[shardIndex]
	shard.Entries = slices.Clone(shard.Entries)
	position := sort.Search(len(shard.Entries), func(position int) bool {
		return shard.Entries[position].Path >= entry.Path
	})
	if created {
		shard.Entries = slices.Insert(shard.Entries, position, *entry)
	} else {
		if position == len(shard.Entries) || shard.Entries[position].Path != entry.Path {
			return nil, MutationResult{}, fmt.Errorf("%w: path %s is missing from shard", storefmt.ErrIntegrity, entry.Path)
		}
		shard.Entries[position] = *entry
	}
	shardID := fmt.Sprintf("%02x", shardIndex)
	if err := validateShardObject(&shard, shardID); err != nil {
		return nil, MutationResult{}, fmt.Errorf("build shard: %w", err)
	}
	shardModel, shardObjectRef, err := immutableJSON(func(hash string) string {
		return shardKey(shardID, hash)
	}, shard)
	if err != nil {
		return nil, MutationResult{}, fmt.Errorf("build shard: %w", err)
	}
	objects = append(objects, shardModel)

	root := base.Root
	root.Shards = slices.Clone(root.Shards)
	root.Shards[shardIndex] = shardRef{Shard: shardID, objectRef: shardObjectRef}
	if created {
		root.DocumentCount++
	}
	if err := validateRootObject(&root, base.Head.WorldID); err != nil {
		return nil, MutationResult{}, fmt.Errorf("build root: %w", err)
	}
	rootModel, rootRef, err := immutableJSON(rootKey, root)
	if err != nil {
		return nil, MutationResult{}, fmt.Errorf("build root: %w", err)
	}
	objects = append(objects, rootModel)

	head := nextHead(&base.Head, rootRef, operationID)
	if err := validateHeadObject(&head); err != nil {
		return nil, MutationResult{}, fmt.Errorf("build head: %w", err)
	}
	headData, err := marshalImmutable(head)
	if err != nil {
		return nil, MutationResult{}, fmt.Errorf("build head: %w", err)
	}
	shards := new([shardCount]shardObject)
	*shards = *base.Shards
	shards[shardIndex] = shard
	preparedAttributes := base.HeadAttributes
	preparedAttributes.Size = int64(len(headData))
	prepared, err := buildDerivedSnapshot(&head, preparedAttributes, &root, shards)
	if err != nil {
		return nil, MutationResult{}, fmt.Errorf("prepare committed snapshot: %w", err)
	}
	var fresh map[string][]byte
	if body != nil {
		fresh = map[string][]byte{entry.Path: body}
	}
	if err := store.indexSections(ctx, prepared, base, fresh, 1); err != nil {
		return nil, MutationResult{}, fmt.Errorf("prepare committed snapshot: %w", err)
	}
	return &candidateMutation{
		base:        base,
		head:        head,
		headData:    headData,
		root:        root,
		shard:       shard,
		shardIndex:  shardIndex,
		objects:     objects,
		result:      result,
		operationID: operationID,
		prepared:    prepared,
	}, result, nil
}

func nextHead(current *headObject, root objectRef, operationID string) headObject {
	sequence := current.Sequence + 1
	receipts := append(slices.Clone(current.Receipts), operationReceipt{
		OperationID: operationID,
		Sequence:    sequence,
		Result:      "committed",
	})
	if len(receipts) > maximumReceipts {
		receipts = slices.Clone(receipts[len(receipts)-maximumReceipts:])
	}
	return headObject{
		Schema:   schemaVersion,
		WorldID:  current.WorldID,
		Sequence: sequence,
		Root:     root,
		Receipts: receipts,
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

func pathHashBytes(path string) [32]byte {
	return sha256.Sum256([]byte(path))
}
