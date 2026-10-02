package bucketstore

import (
	"fmt"
	"testing"
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
