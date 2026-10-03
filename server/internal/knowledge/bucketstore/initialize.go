package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

// initialize creates the deterministic genesis: an empty root, checkpoint
// zero naming it, and last the world marker, so a marker always has a
// checkpoint. A world that exists already is validated instead.
func initialize(ctx context.Context, objects blob.Store, worldID string) error {
	if ctx == nil {
		return fmt.Errorf("initialize bucket store: %w: context is nil", blob.ErrPrecondition)
	}
	if nilStore(objects) {
		return fmt.Errorf("initialize bucket store: %w: blob store is nil", blob.ErrPrecondition)
	}
	if !validUUID(worldID) {
		return fmt.Errorf("initialize bucket store: %w: invalid world ID %q", blob.ErrPrecondition, worldID)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("initialize bucket store: %w", err)
	}

	_, err := objects.Get(ctx, markerKey)
	switch {
	case err == nil:
		return validateExistingWorld(ctx, objects, worldID)
	case !errors.Is(err, blob.ErrNotFound):
		return fmt.Errorf("initialize bucket store: check marker: %w", err)
	}

	model, err := buildGenesis(worldID)
	if err != nil {
		return fmt.Errorf("initialize bucket store: %w", err)
	}
	for _, object := range model[:len(model)-1] {
		if err := createImmutable(ctx, objects, object); err != nil {
			return fmt.Errorf("initialize bucket store: %w", err)
		}
	}
	marker := model[len(model)-1]
	if err := createMarker(ctx, objects, worldID, marker); err != nil {
		return fmt.Errorf("initialize bucket store: %w", err)
	}
	return nil
}

// buildGenesis is the empty world's objects, the marker last: one empty
// folded shard, its root, checkpoint zero.
func buildGenesis(worldID string) ([]modelObject, error) {
	shard, ref, err := foldShard(0, 0, make([]foldedEntry, 0))
	if err != nil {
		return nil, err
	}
	rootModel, rootRef, err := foldRoot(worldID, 0, 0, []shardRef{ref})
	if err != nil {
		return nil, err
	}
	checkpoint := checkpointObject{Schema: logSchema, WorldID: worldID, Sequence: 1, Root: rootRef}
	if err := validateCheckpoint(&checkpoint, 1); err != nil {
		return nil, fmt.Errorf("build checkpoint zero: %w", err)
	}
	checkpointData, err := marshalImmutable(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("build checkpoint zero: %w", err)
	}
	marker := markerObject{Schema: logSchema, WorldID: worldID}
	if err := validateMarker(&marker); err != nil {
		return nil, fmt.Errorf("build marker: %w", err)
	}
	markerData, err := marshalImmutable(marker)
	if err != nil {
		return nil, fmt.Errorf("build marker: %w", err)
	}
	return []modelObject{shard, rootModel, {Key: checkpointKey(1), Data: checkpointData}, {Key: markerKey, Data: markerData}}, nil
}

func createImmutable(ctx context.Context, objects blob.Store, object modelObject) error {
	existing, err := createOrRead(ctx, objects, object, true)
	if err == nil && existing != nil {
		return fmt.Errorf("%w: immutable object %q contains hash %s, want %s", blob.ErrIntegrity, object.Key, hashHex(existing.Data), hashHex(object.Data))
	}
	return err
}

// freshenAge is how old stored bytes may be for a create to reuse them as
// they are; older ones are rewritten first, which fences a pending drop. It
// is fixed, so replicas configured with different graces stay fenced.
const freshenAge = 15 * time.Minute / 2

// createOrRead creates an immutable object, an unknown outcome decided by
// reading the name back; other bytes there are returned. With freshen, equal
// bytes older than freshenAge are rewritten first.
func createOrRead(ctx context.Context, objects blob.Store, object modelObject, freshen bool) (*blob.Object, error) {
	var lastErr error
	for attempt := range maximumCreateAttempts {
		_, err := objects.Create(ctx, object.Key, object.Data)
		if err == nil {
			return nil, nil
		}
		lastErr = err
		if !errors.Is(err, blob.ErrPrecondition) && !retryableObjectError(err) {
			return nil, fmt.Errorf("create immutable object %q: %w", object.Key, err)
		}

		existing, getErr := objects.Get(ctx, object.Key)
		if getErr == nil {
			if err := validateReadObject(object.Key, &existing); err != nil {
				return nil, fmt.Errorf("reconcile immutable object %q: %w", object.Key, err)
			}
			if !bytes.Equal(existing.Data, object.Data) {
				return &existing, nil
			}
			if !freshen || time.Since(existing.Attributes.Modified) < freshenAge {
				return nil, nil
			}
			// A new generation fails a delete conditioned on the one it read; a
			// delete that already won makes the next attempt create it again.
			_, getErr = objects.Replace(ctx, object.Key, existing.Attributes.Generation, object.Data)
			if getErr == nil {
				return nil, nil
			}
			if errors.Is(getErr, blob.ErrPrecondition) {
				continue
			}
		}
		lastErr = errors.Join(lastErr, getErr)
		if !errors.Is(getErr, blob.ErrNotFound) && !retryableObjectError(getErr) {
			return nil, fmt.Errorf("reconcile immutable object %q: %w", object.Key, lastErr)
		}
		if attempt == maximumCreateAttempts-1 {
			break
		}
		if err := waitForRetry(ctx, createRetryDelay(attempt)); err != nil {
			return nil, fmt.Errorf("wait to retry immutable object %q: %w", object.Key, errors.Join(lastErr, err))
		}
	}
	return nil, fmt.Errorf("create immutable object %q retries exhausted: %w", object.Key, lastErr)
}

// createMarker creates the world marker; one already there must be this
// world's, else the bucket belongs to another world.
func createMarker(ctx context.Context, objects blob.Store, worldID string, marker modelObject) error {
	err := createImmutable(ctx, objects, marker)
	if errors.Is(err, blob.ErrIntegrity) {
		if existing := validateExistingWorld(ctx, objects, worldID); existing != nil {
			return existing
		}
	}
	return err
}

// validateExistingWorld checks the bucket holds worldID's marker and a
// checkpoint that loads. The caller opens the world again to serve it.
func validateExistingWorld(ctx context.Context, objects blob.Store, worldID string) error {
	if _, err := loadBase(ctx, objects, worldID, defaultShardWorkers); err != nil {
		return fmt.Errorf("validate existing world: %w", err)
	}
	return nil
}
