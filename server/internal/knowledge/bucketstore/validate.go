package bucketstore

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
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

func validateHeadObject(head *headObject) error {
	if head.Schema != schemaVersion {
		return fmt.Errorf("schema is %d, want %d", head.Schema, schemaVersion)
	}
	if !validWorldID(head.WorldID) {
		return fmt.Errorf("invalid world ID %q", head.WorldID)
	}
	if head.Sequence < 1 {
		return fmt.Errorf("sequence must be positive")
	}
	if head.Receipts == nil {
		return fmt.Errorf("receipts must be an array")
	}
	expectedReceipts := int(min(head.Sequence-1, int64(maximumReceipts)))
	if len(head.Receipts) != expectedReceipts {
		return fmt.Errorf("receipt count is %d, want %d at sequence %d", len(head.Receipts), expectedReceipts, head.Sequence)
	}
	if err := verifyRef(head.Root, rootKey(head.Root.Hash)); err != nil {
		return fmt.Errorf("root reference: %w", err)
	}
	seen := make(map[string]struct{}, len(head.Receipts))
	for index, receipt := range head.Receipts {
		expectedSequence := head.Sequence - int64(len(head.Receipts)) + int64(index) + 1
		if !validWorldID(receipt.OperationID) {
			return fmt.Errorf("receipt %d has invalid operation ID %q", index, receipt.OperationID)
		}
		if receipt.Sequence != expectedSequence {
			return fmt.Errorf("receipt %d has sequence %d, want %d", index, receipt.Sequence, expectedSequence)
		}
		if receipt.Result != "committed" {
			return fmt.Errorf("receipt %d has unsupported result %q", index, receipt.Result)
		}
		if _, exists := seen[receipt.OperationID]; exists {
			return fmt.Errorf("receipt %d duplicates operation ID %q", index, receipt.OperationID)
		}
		seen[receipt.OperationID] = struct{}{}
	}
	return nil
}

func validateRootObject(root *rootObject, expectedWorldID string) error {
	if root.Schema != schemaVersion {
		return fmt.Errorf("schema is %d, want %d", root.Schema, schemaVersion)
	}
	if !validWorldID(root.WorldID) {
		return fmt.Errorf("invalid world ID %q", root.WorldID)
	}
	if expectedWorldID != "" && root.WorldID != expectedWorldID {
		return fmt.Errorf("world ID %q does not match head world %q", root.WorldID, expectedWorldID)
	}
	if root.DocumentCount < 0 || root.DocumentCount > maximumDocuments {
		return fmt.Errorf("document count %d is outside [0,%d]", root.DocumentCount, maximumDocuments)
	}
	if root.Shards == nil {
		return fmt.Errorf("shards must be an array")
	}
	if len(root.Shards) != shardCount {
		return fmt.Errorf("shard count is %d, want %d", len(root.Shards), shardCount)
	}
	for index, ref := range root.Shards {
		expectedShard := fmt.Sprintf("%02x", index)
		if ref.Shard != expectedShard {
			return fmt.Errorf("shard reference %d is labeled %q, want %q", index, ref.Shard, expectedShard)
		}
		if err := verifyRef(ref.objectRef, shardKey(expectedShard, ref.Hash)); err != nil {
			return fmt.Errorf("shard reference %s: %w", expectedShard, err)
		}
	}
	return nil
}

func validateShardObject(shard *shardObject, expectedShard string) error {
	if shard.Schema != schemaVersion {
		return fmt.Errorf("schema is %d, want %d", shard.Schema, schemaVersion)
	}
	if shard.Shard != expectedShard {
		return fmt.Errorf("shard label is %q, want %q", shard.Shard, expectedShard)
	}
	shardIndex, err := strconv.ParseUint(expectedShard, 16, 8)
	if err != nil || fmt.Sprintf("%02x", shardIndex) != expectedShard {
		return fmt.Errorf("invalid expected shard %q", expectedShard)
	}
	if shard.Entries == nil {
		return fmt.Errorf("entries must be an array")
	}
	for index := range shard.Entries {
		if index > 0 && shard.Entries[index-1].Path >= shard.Entries[index].Path {
			return fmt.Errorf("entries %d and %d are not strictly path-sorted", index-1, index)
		}
		if err := validateShardEntry(&shard.Entries[index], int(shardIndex)); err != nil {
			return fmt.Errorf("entry %d: %w", index, err)
		}
	}
	return nil
}

func validateShardEntry(entry *shardEntry, shardIndex int) error {
	if entry.Path == "/" || protocol.ValidateRequestPath(entry.Path) != nil || protocolstore.ContainsDotDot(entry.Path) || protocolstore.CanonicalPath(entry.Path) != entry.Path {
		return fmt.Errorf("path %q is not a canonical document path", entry.Path)
	}
	expectedPathHash := pathHash(entry.Path)
	if entry.PathHash != expectedPathHash {
		return fmt.Errorf("path hash %q does not match path %q", entry.PathHash, entry.Path)
	}
	pathSum, err := strconv.ParseUint(entry.PathHash[:2], 16, 8)
	if err != nil || int(pathSum) != shardIndex {
		return fmt.Errorf("path %q belongs to shard %02x, not %02x", entry.Path, pathSum, shardIndex)
	}
	if err := verifyRef(entry.Manifest, manifestKey(entry.PathHash, entry.Manifest.Hash)); err != nil {
		return fmt.Errorf("manifest reference: %w", err)
	}
	if entry.Current < 1 || entry.Current > protocolstore.MaxVersionNumber {
		return fmt.Errorf("current version is outside [1,%d]", protocolstore.MaxVersionNumber)
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
	if err := protocolstore.ValidateMeta(record.Metadata); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if record.Title == "" {
		return fmt.Errorf("title must not be empty")
	}
	expectedTags := catalog.ParseTags(record.Metadata["tags"])
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
	if history.Schema != schemaVersion {
		return fmt.Errorf("schema is %d, want %d", history.Schema, schemaVersion)
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

func validateManifestObject(manifest *manifestObject) error {
	if manifest.Schema != schemaVersion {
		return fmt.Errorf("schema is %d, want %d", manifest.Schema, schemaVersion)
	}
	if !validHash(manifest.PathHash) {
		return fmt.Errorf("invalid path hash %q", manifest.PathHash)
	}
	if manifest.Current < 1 || manifest.Current > protocolstore.MaxVersionNumber {
		return fmt.Errorf("current version is outside [1,%d]", protocolstore.MaxVersionNumber)
	}
	if manifest.History == nil {
		return fmt.Errorf("history must be an array")
	}
	if len(manifest.History) == 0 {
		return fmt.Errorf("history must not be empty")
	}
	previousLast := 0
	previousBlock := -1
	for index, ref := range manifest.History {
		if ref.PathHash != manifest.PathHash {
			return fmt.Errorf("history reference %d path hash does not match manifest", index)
		}
		if err := validateHistoryRange(ref.First, ref.Last); err != nil {
			return fmt.Errorf("history reference %d: %w", index, err)
		}
		if err := verifyRef(ref.objectRef, historyKey(ref.Hash)); err != nil {
			return fmt.Errorf("history reference %d: %w", index, err)
		}
		block := (ref.First - 1) / historyBlockSize
		if index > 0 && (ref.First != previousLast+1 || block != previousBlock+1) {
			return fmt.Errorf("history reference %d is not contiguous by absolute block", index)
		}
		previousLast = ref.Last
		previousBlock = block
	}
	if previousLast != manifest.Current {
		return fmt.Errorf("history ends at %d, current version is %d", previousLast, manifest.Current)
	}
	return nil
}

func validateHistoryRange(first, last int) error {
	if first < 1 || last < first || last > protocolstore.MaxVersionNumber {
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
