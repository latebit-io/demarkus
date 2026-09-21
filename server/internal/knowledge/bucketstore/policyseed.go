package bucketstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// PolicySeed is a default policy document and the catalog metadata it is
// published with.
type PolicySeed struct {
	Body     []byte
	Metadata map[string]string
}

// ValidatePolicySeed rejects a seed that would not survive a publish:
// wrong document shape, invalid write, or an unenforceable policy.
func ValidatePolicySeed(seed PolicySeed) error {
	if err := storefmt.ValidateDocumentContent(publishpolicy.DocumentPath, seed.Body); err != nil {
		return fmt.Errorf("invalid policy document: %w", err)
	}
	if err := storefmt.ValidateWrite(seed.Body, seed.Metadata); err != nil {
		return fmt.Errorf("invalid policy write: %w", err)
	}
	if err := publishpolicy.Parse(string(seed.Body)).Validate(); err != nil {
		return fmt.Errorf("invalid policy: %w", err)
	}
	return nil
}

// createPolicy publishes seed as the world's policy when it has none and
// reports whether it created one. Create-only, so a restart never reverts
// a policy that replaced an earlier seed.
func (store *Store) createPolicy(ctx context.Context, seed PolicySeed) (bool, error) {
	// An archived entry counts as present so no seed overwrites it.
	if _, exists := store.snapshot.Load().Paths[publishpolicy.DocumentPath]; exists {
		return false, nil
	}
	if err := ValidatePolicySeed(seed); err != nil {
		return false, err
	}
	request := backend.WriteRequest{Path: publishpolicy.DocumentPath, Content: seed.Body, Metadata: seed.Metadata}
	if _, err := store.Publish(ctx, request); err != nil {
		// A concurrent replica won the create; its policy stands.
		if errors.Is(err, storefmt.ErrConflict) {
			return false, nil
		}
		return false, fmt.Errorf("create policy: %w", err)
	}
	return true, nil
}

// EnsureWorld creates the world's genesis when the bucket is empty and
// reports whether it did. Objects but no head is refused as a misconfigured
// bucket URL: an advisory check, not a lock against foreign writers.
func EnsureWorld(ctx context.Context, objects blob.Store, worldID string) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("ensure world: %w: context is nil", blob.ErrPrecondition)
	}
	if nilStore(objects) {
		return false, fmt.Errorf("ensure world: %w: blob store is nil", blob.ErrPrecondition)
	}
	_, err := objects.Head(ctx, headObjectKey)
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, blob.ErrNotFound):
		return false, fmt.Errorf("ensure world: check head: %w", err)
	}
	listed, err := objects.List(ctx, "", "")
	if err != nil {
		return false, fmt.Errorf("ensure world: list bucket: %w", err)
	}
	if len(listed.Objects) > 0 {
		return false, fmt.Errorf("ensure world: %w: bucket holds %d objects but no world head",
			blob.ErrPrecondition, len(listed.Objects))
	}
	if err := Initialize(ctx, objects, worldID); err != nil {
		return false, err
	}
	return true, nil
}

// seedPolicy publishes the seed when the world has no policy document.
func (store *Store) seedPolicy(ctx context.Context, seed PolicySeed) error {
	created, err := store.createPolicy(ctx, seed)
	if err != nil {
		return err
	}
	if created {
		store.logger.Info("seeded the initial write policy",
			"world_id", store.worldID, "path", publishpolicy.DocumentPath)
	}
	return nil
}
