package bucketstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

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
