package bucketstore

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

func validHash(hash string) bool {
	if len(hash) != 2*32 {
		return false
	}
	for _, character := range []byte(hash) {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func validWorldID(worldID string) bool {
	if len(worldID) != 36 {
		return false
	}
	for index, character := range []byte(worldID) {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return false
			}
		}
	}
	if worldID[19] != '8' && worldID[19] != '9' && worldID[19] != 'a' && worldID[19] != 'b' {
		return false
	}
	return true
}

func verifyRef(ref objectRef, expectedKey string) error {
	if !validHash(ref.Hash) {
		return fmt.Errorf("invalid SHA-256 hash %q", ref.Hash)
	}
	if ref.Key != expectedKey {
		return fmt.Errorf("reference key %q does not match hash-derived key %q", ref.Key, expectedKey)
	}
	return nil
}

func parseTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	if parsed.UTC().Format(time.RFC3339) != value {
		return time.Time{}, fmt.Errorf("timestamp %q is not canonical UTC RFC3339 seconds", value)
	}
	return parsed.UTC(), nil
}

func validTimestamp(value string) bool {
	_, err := parseTimestamp(value)
	return err == nil
}

func validateMarker(marker *markerObject) error {
	if marker.Schema != logSchema {
		return fmt.Errorf("schema is %d, want %d", marker.Schema, logSchema)
	}
	if !validWorldID(marker.WorldID) {
		return fmt.Errorf("invalid world ID %q", marker.WorldID)
	}
	return nil
}

// validateCheckpoint checks a checkpoint read at the key for sequence.
func validateCheckpoint(checkpoint *checkpointObject, sequence int64) error {
	if checkpoint.Schema != logSchema {
		return fmt.Errorf("schema is %d, want %d", checkpoint.Schema, logSchema)
	}
	if !validWorldID(checkpoint.WorldID) {
		return fmt.Errorf("invalid world ID %q", checkpoint.WorldID)
	}
	if checkpoint.Sequence != sequence {
		return fmt.Errorf("sequence is %d, key names %d", checkpoint.Sequence, sequence)
	}
	if err := verifyRef(checkpoint.Root, rootKey(checkpoint.Root.Hash)); err != nil {
		return fmt.Errorf("root reference: %w", err)
	}
	if checkpoint.Tip != "" && !validHash(checkpoint.Tip) {
		return fmt.Errorf("invalid tip %q", checkpoint.Tip)
	}
	return nil
}

// validateSlot checks a slot read at the key for first; the chain and the
// world are checked against the snapshot it extends.
func validateSlot(slot *slotObject, first int64) error {
	if slot.Schema != logSchema {
		return fmt.Errorf("schema is %d, want %d", slot.Schema, logSchema)
	}
	if !validWorldID(slot.WorldID) {
		return fmt.Errorf("invalid world ID %q", slot.WorldID)
	}
	if slot.First != first || first < 2 {
		return fmt.Errorf("first sequence is %d, key names %d", slot.First, first)
	}
	if !validWorldID(slot.Store) {
		return fmt.Errorf("invalid store ID %q", slot.Store)
	}
	if slot.Prev != "" && !validHash(slot.Prev) {
		return fmt.Errorf("invalid predecessor hash %q", slot.Prev)
	}
	if len(slot.Entries) == 0 || len(slot.Entries) > maxSlotEntries {
		return fmt.Errorf("entry count %d is outside [1,%d]", len(slot.Entries), maxSlotEntries)
	}
	seen := make(map[string]struct{}, len(slot.Entries))
	for index := range slot.Entries {
		entry := &slot.Entries[index]
		if _, exists := seen[entry.OperationID]; exists {
			return fmt.Errorf("entry %d duplicates operation ID %q", index, entry.OperationID)
		}
		seen[entry.OperationID] = struct{}{}
		if err := validateSlotEntry(entry); err != nil {
			return fmt.Errorf("entry %d: %w", index, err)
		}
	}
	return nil
}

func validateSlotEntry(entry *slotEntry) error {
	if !validWorldID(entry.OperationID) {
		return fmt.Errorf("invalid operation ID %q", entry.OperationID)
	}
	if err := validateDocumentPath(entry.Path); err != nil {
		return err
	}
	if !protocol.IsKnownOp(entry.Op) {
		return fmt.Errorf("unknown op %q", entry.Op)
	}
	if !protocol.IsValidMetaValue(entry.Agent) {
		return errors.New("agent is not a valid metadata value")
	}
	if entry.Current < 1 || entry.Current > storefmt.MaxVersionNumber {
		return fmt.Errorf("current version is outside [1,%d]", storefmt.MaxVersionNumber)
	}
	if entry.First < 1 || entry.First > entry.Current {
		return fmt.Errorf("first retained version %d is outside [1,%d]", entry.First, entry.Current)
	}
	if !validBodyHash(entry.BodyHash) {
		return fmt.Errorf("invalid body hash %q", entry.BodyHash)
	}
	if _, err := parseTimestamp(entry.Modified); err != nil {
		return fmt.Errorf("modified: %w", err)
	}
	if entry.Version == nil {
		// An archive transition: the op names its direction, as a WATCH event does.
		if entry.Catalog != nil {
			return errors.New("an archive transition carries no catalog record")
		}
		if entry.Op != changefeed.ArchiveOp(entry.Archived) {
			return fmt.Errorf("op %q does not match archived %t", entry.Op, entry.Archived)
		}
		return nil
	}
	if entry.Op != protocol.OpPublish && entry.Op != protocol.OpAppend {
		return fmt.Errorf("a write has op %q", entry.Op)
	}
	if entry.Archived {
		return errors.New("a write leaves the document archived")
	}
	if entry.Catalog == nil {
		return errors.New("a write carries no catalog record")
	}
	if err := validateCatalogRecord(entry.Catalog, entry.Path, entry.Modified); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	version := entry.Version
	if version.Version != entry.Current || version.BodyHash != entry.BodyHash || version.Modified != entry.Modified {
		return errors.New("version record does not match the entry")
	}
	if err := verifyRef(version.Blob, blobKey(version.Blob.Hash)); err != nil {
		return fmt.Errorf("blob reference: %w", err)
	}
	return nil
}

func validateDocumentPath(path string) error {
	if path == "/" || protocol.ValidateRequestPath(path) != nil || storefmt.ContainsDotDot(path) || storefmt.CanonicalPath(path) != path {
		return fmt.Errorf("path %q is not a canonical document path", path)
	}
	return nil
}

func validateCatalogRecord(record *catalogRecord, expectedPath, expectedModified string) error {
	if record.Path != expectedPath {
		return fmt.Errorf("path %q does not match shard path %q", record.Path, expectedPath)
	}
	if record.Modified != expectedModified {
		return fmt.Errorf("modified %q does not match shard modified %q", record.Modified, expectedModified)
	}
	if _, err := parseTimestamp(record.Modified); err != nil {
		return fmt.Errorf("modified: %w", err)
	}
	if record.Tags == nil {
		return fmt.Errorf("tags must be an array")
	}
	if record.Metadata == nil {
		return fmt.Errorf("metadata must be an object")
	}
	if err := storefmt.ValidateMeta(record.Metadata); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if record.Title == "" {
		return fmt.Errorf("title must not be empty")
	}
	expectedTags := protocol.SplitTags(record.Metadata["tags"])
	if !slices.Equal(record.Tags, expectedTags) {
		return fmt.Errorf("tags do not match metadata tags")
	}
	importance, err := parseCanonicalImportance(record.Importance)
	if err != nil {
		return err
	}
	if importance != catalog.ParseImportance(record.Metadata["importance"]) {
		return fmt.Errorf("importance %q does not match metadata importance", record.Importance)
	}
	if declaredTitle := strings.TrimSpace(record.Metadata["title"]); declaredTitle != "" && record.Title != declaredTitle {
		return fmt.Errorf("title %q does not match declared title %q", record.Title, declaredTitle)
	}
	return nil
}

func validateHistoryObject(history *historyObject) error {
	if history.Schema != historySchema {
		return fmt.Errorf("schema is %d, want %d", history.Schema, historySchema)
	}
	if !validHash(history.PathHash) {
		return fmt.Errorf("invalid path hash %q", history.PathHash)
	}
	if err := validateHistoryRange(history.First, history.Last); err != nil {
		return err
	}
	if history.Entries == nil {
		return fmt.Errorf("entries must be an array")
	}
	if len(history.Entries) != history.Last-history.First+1 {
		return fmt.Errorf("entry count %d does not match range %d-%d", len(history.Entries), history.First, history.Last)
	}
	for index, entry := range history.Entries {
		expectedVersion := history.First + index
		if entry.Version != expectedVersion {
			return fmt.Errorf("entry %d version is %d, want %d", index, entry.Version, expectedVersion)
		}
		if err := verifyRef(entry.Blob, blobKey(entry.Blob.Hash)); err != nil {
			return fmt.Errorf("entry %d blob reference: %w", index, err)
		}
		if !validBodyHash(entry.BodyHash) {
			return fmt.Errorf("entry %d has invalid body hash %q", index, entry.BodyHash)
		}
		if _, err := parseTimestamp(entry.Modified); err != nil {
			return fmt.Errorf("entry %d modified: %w", index, err)
		}
	}
	return nil
}

// validateBlocks checks a document's history blocks are contiguous, one per
// absolute block, and end at current.
func validateBlocks(blocks []blockRef, current int) error {
	if len(blocks) == 0 {
		return fmt.Errorf("history must not be empty")
	}
	previousLast := 0
	previousBlock := -1
	for index, ref := range blocks {
		if err := validateHistoryRange(ref.First, ref.Last); err != nil {
			return fmt.Errorf("history reference %d: %w", index, err)
		}
		if !validHash(ref.Hash) {
			return fmt.Errorf("history reference %d: invalid SHA-256 hash %q", index, ref.Hash)
		}
		block := (ref.First - 1) / historyBlockSize
		if index > 0 && (ref.First != previousLast+1 || block != previousBlock+1) {
			return fmt.Errorf("history reference %d is not contiguous by absolute block", index)
		}
		previousLast = ref.Last
		previousBlock = block
	}
	if previousLast != current {
		return fmt.Errorf("history ends at %d, current version is %d", previousLast, current)
	}
	return nil
}

// validateFoldedRoot checks a folded root; its shard count must be the one
// its document count gives, as every compactor computes it.
func validateFoldedRoot(root *foldedRoot, expectedWorldID string) error {
	if root.Schema != foldedSchema {
		return fmt.Errorf("schema is %d, want %d", root.Schema, foldedSchema)
	}
	if !validWorldID(root.WorldID) {
		return fmt.Errorf("invalid world ID %q", root.WorldID)
	}
	if root.WorldID != expectedWorldID {
		return fmt.Errorf("world ID %q does not match world %q", root.WorldID, expectedWorldID)
	}
	if root.DocumentCount < 0 || root.DocumentCount > docsPerShard<<maxShardBits {
		return fmt.Errorf("document count %d is outside [0,%d]", root.DocumentCount, docsPerShard<<maxShardBits)
	}
	if want := shardBitsFor(root.DocumentCount); root.ShardBits != want {
		return fmt.Errorf("shard bits are %d, want %d for %d documents", root.ShardBits, want, root.DocumentCount)
	}
	if len(root.Shards) != 1<<root.ShardBits {
		return fmt.Errorf("shard count is %d, want %d", len(root.Shards), 1<<root.ShardBits)
	}
	for index, ref := range root.Shards {
		label := shardLabel(index, root.ShardBits)
		if ref.Shard != label {
			return fmt.Errorf("shard reference %d is labeled %q, want %q", index, ref.Shard, label)
		}
		if err := verifyRef(ref.objectRef, shardKey(label, ref.Hash)); err != nil {
			return fmt.Errorf("shard reference %s: %w", label, err)
		}
	}
	return nil
}

func validateFoldedShard(shard *foldedShard, index, bits int) error {
	if shard.Schema != foldedSchema {
		return fmt.Errorf("schema is %d, want %d", shard.Schema, foldedSchema)
	}
	if shard.ShardBits != bits {
		return fmt.Errorf("shard bits are %d, root has %d", shard.ShardBits, bits)
	}
	if label := shardLabel(index, bits); shard.Shard != label {
		return fmt.Errorf("shard label is %q, want %q", shard.Shard, label)
	}
	if shard.Entries == nil {
		return fmt.Errorf("entries must be an array")
	}
	for entryIndex := range shard.Entries {
		if entryIndex > 0 && shard.Entries[entryIndex-1].Path >= shard.Entries[entryIndex].Path {
			return fmt.Errorf("entries %d and %d are not strictly path-sorted", entryIndex-1, entryIndex)
		}
		if err := validateFoldedEntry(&shard.Entries[entryIndex], index, bits); err != nil {
			return fmt.Errorf("entry %d: %w", entryIndex, err)
		}
	}
	return nil
}

func validateFoldedEntry(entry *foldedEntry, index, bits int) error {
	if err := validateDocumentPath(entry.Path); err != nil {
		return err
	}
	if entry.PathHash != pathHash(entry.Path) {
		return fmt.Errorf("path hash %q does not match path %q", entry.PathHash, entry.Path)
	}
	if shard := shardOf(entry.PathHash, bits); shard != index {
		return fmt.Errorf("path %q belongs to shard %d, not %d", entry.Path, shard, index)
	}
	if entry.Current < 1 || entry.Current > storefmt.MaxVersionNumber {
		return fmt.Errorf("current version is outside [1,%d]", storefmt.MaxVersionNumber)
	}
	if !validBodyHash(entry.BodyHash) {
		return fmt.Errorf("invalid body hash %q", entry.BodyHash)
	}
	if _, err := parseTimestamp(entry.Modified); err != nil {
		return fmt.Errorf("modified: %w", err)
	}
	if err := validateCatalogRecord(&entry.Catalog, entry.Path, entry.Modified); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	return validateBlocks(entry.History, entry.Current)
}

func validateHistoryRange(first, last int) error {
	if first < 1 || last < first || last > storefmt.MaxVersionNumber {
		return fmt.Errorf("invalid history range %d-%d", first, last)
	}
	if (first-1)/historyBlockSize != (last-1)/historyBlockSize {
		return fmt.Errorf("history range %d-%d crosses an absolute %d-version block", first, last, historyBlockSize)
	}
	return nil
}

func validBodyHash(hash string) bool {
	return strings.HasPrefix(hash, "sha256-") && validHash(strings.TrimPrefix(hash, "sha256-"))
}

func parseCanonicalImportance(value string) (float64, error) {
	if value == "" {
		return 0, fmt.Errorf("importance must be a canonical decimal string")
	}
	dot := false
	for _, character := range []byte(value) {
		switch {
		case character >= '0' && character <= '9':
		case character == '.' && !dot:
			dot = true
		default:
			return 0, fmt.Errorf("importance %q is not a decimal string", value)
		}
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 || parsed > 1 {
		return 0, fmt.Errorf("importance %q is outside canonical range [0,1]", value)
	}
	if strconv.FormatFloat(parsed, 'f', -1, 64) != value {
		return 0, fmt.Errorf("importance %q is not the canonical round-tripping decimal", value)
	}
	return parsed, nil
}
