package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
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
	if err := protocolstore.ValidateDocumentContent(publishpolicy.DocumentPath, seed.Body); err != nil {
		return fmt.Errorf("invalid policy document: %w", err)
	}
	if err := protocolstore.ValidateWrite(seed.Body, seed.Metadata); err != nil {
		return fmt.Errorf("invalid policy write: %w", err)
	}
	if err := publishpolicy.Parse(string(seed.Body)).Validate(); err != nil {
		return fmt.Errorf("invalid policy: %w", err)
	}
	return nil
}

// SeedPolicy publishes seed as the world's policy when it has none and
// reports whether it created one. Create-only, so a restart never reverts
// a policy that replaced an earlier seed.
func (store *Store) SeedPolicy(seed PolicySeed) (bool, error) {
	if err := ValidatePolicySeed(seed); err != nil {
		return false, err
	}
	_, err := store.Get(publishpolicy.DocumentPath, 0)
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("check existing policy: %w", err)
	}
	if _, err := store.WriteVersion(publishpolicy.DocumentPath, 0, seed.Body, seed.Metadata); err != nil {
		// A concurrent replica won the create; its policy stands.
		if errors.Is(err, protocolstore.ErrConflict) {
			return false, nil
		}
		return false, fmt.Errorf("create policy: %w", err)
	}
	return true, nil
}

// EnsureWorld creates the world's genesis when the bucket is empty and
// reports whether it did. Objects but no world head is refused: that is
// another bucket, not a new world, usually a misconfigured bucket URL.
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

// ensurePolicy makes the stored policy usable before enforcement starts:
// seed it when absent and a seed is configured, then validate whatever
// the world actually holds.
func (store *Store) ensurePolicy(ctx context.Context, seed *PolicySeed) error {
	if seed != nil {
		created, err := store.SeedPolicy(*seed)
		if err != nil {
			return err
		}
		if created {
			store.logger.Info("seeded the default write policy",
				"world", store.worldID, "path", publishpolicy.DocumentPath)
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	view := &readView{ctx: requestCtx, objects: store.objects, snapshot: store.snapshot.Load()}
	if _, err := view.currentPolicy(true); err != nil {
		return err
	}
	return nil
}
