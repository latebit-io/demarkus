package bucketstore

import (
	"fmt"
	"testing"

	"github.com/latebit-io/demarkus/server/blob"
)

// legacyHead is head.json as the release before the commit log decoded it,
// copied so a test can play an old replica against the schema 2 marker.
type legacyHead struct {
	Schema   int             `json:"schema"`
	WorldID  string          `json:"world_id"`
	Sequence int64           `json:"sequence"`
	Root     objectRef       `json:"root"`
	Receipts []legacyReceipt `json:"receipts"`
}

type legacyReceipt struct {
	OperationID string `json:"operation_id"`
	Sequence    int64  `json:"sequence"`
	Result      string `json:"result"`
	Path        string `json:"path,omitempty"`
	Op          string `json:"op,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Version     int    `json:"version,omitempty"`
	Hash        string `json:"hash,omitempty"`
}

// validateLegacyHead is the old release's check, up to the root reference.
func validateLegacyHead(head *legacyHead) error {
	if head.Schema != 1 {
		return fmt.Errorf("schema is %d, want 1", head.Schema)
	}
	if !validWorldID(head.WorldID) || head.Sequence < 1 || head.Receipts == nil {
		return fmt.Errorf("invalid head %+v", head)
	}
	return verifyRef(head.Root, rootKey(head.Root.Hash))
}

// A replica on the release before the commit log reads the world marker as
// its head and fails closed, so it can never commit to a migrated world.
func TestOldReplicaRefusesTheMarker(t *testing.T) {
	objects := initializedMemory(t)
	marker := getObject(t, objects, markerKey)
	var head legacyHead
	decodeErr := decodeImmutable(marker.Data, &head)
	if decodeErr == nil {
		if err := validateLegacyHead(&head); err == nil {
			t.Fatalf("the old release accepted the marker as head %+v", head)
		}
	}
}

// legacyMemory is a world whose newest checkpoint names a schema 1 root, as a
// migrated world's first checkpoint does: 256 empty shards.
func legacyMemory(t *testing.T) *blob.Memory {
	t.Helper()
	objects := initializedMemory(t)
	refs := make([]shardRef, shardCount)
	for index := range shardCount {
		label := fmt.Sprintf("%02x", index)
		shard := shardObject{Schema: schemaVersion, Shard: label, Entries: make([]shardEntry, 0)}
		model, ref, err := immutableJSON(func(hash string) string { return shardKey(label, hash) }, shard)
		if err != nil {
			t.Fatalf("build shard %s: %v", label, err)
		}
		createReadObject(t, objects, model)
		refs[index] = shardRef{Shard: label, objectRef: ref}
	}
	installRoot(t, objects, rootObject{Schema: schemaVersion, WorldID: testWorldID, Shards: refs})
	return objects
}
