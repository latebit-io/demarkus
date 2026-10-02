package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"

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
	if !validWorldID(worldID) {
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

// buildGenesis is the empty world's objects, the marker last.
func buildGenesis(worldID string) ([]modelObject, error) {
	objects := make([]modelObject, 0, shardCount+3)
	refs := make([]shardRef, 0, shardCount)
	for index := range shardCount {
		shardID := fmt.Sprintf("%02x", index)
		shard := shardObject{Schema: schemaVersion, Shard: shardID, Entries: make([]shardEntry, 0)}
		if err := validateShardObject(&shard, shardID); err != nil {
			return nil, fmt.Errorf("build shard %s: %w", shardID, err)
		}
		object, ref, err := immutableJSON(func(hash string) string {
			return shardKey(shardID, hash)
		}, shard)
		if err != nil {
			return nil, fmt.Errorf("build shard %s: %w", shardID, err)
		}
		objects = append(objects, object)
		refs = append(refs, shardRef{Shard: shardID, objectRef: ref})
	}
	root := rootObject{
		Schema:        schemaVersion,
		WorldID:       worldID,
		DocumentCount: 0,
		Shards:        refs,
	}
	if err := validateRootObject(&root, worldID); err != nil {
		return nil, fmt.Errorf("build root: %w", err)
	}
	rootModel, rootRef, err := immutableJSON(rootKey, root)
	if err != nil {
		return nil, fmt.Errorf("build root: %w", err)
	}
	objects = append(objects, rootModel)
	checkpoint := checkpointObject{Schema: logSchema, WorldID: worldID, Sequence: 1, Root: rootRef}
	if err := validateCheckpoint(&checkpoint, 1); err != nil {
		return nil, fmt.Errorf("build checkpoint zero: %w", err)
	}
	checkpointData, err := marshalImmutable(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("build checkpoint zero: %w", err)
	}
	objects = append(objects, modelObject{Key: checkpointKey(1), Data: checkpointData})
	marker := markerObject{Schema: logSchema, WorldID: worldID}
	if err := validateMarker(&marker); err != nil {
		return nil, fmt.Errorf("build marker: %w", err)
	}
	markerData, err := marshalImmutable(marker)
	if err != nil {
		return nil, fmt.Errorf("build marker: %w", err)
	}
	return append(objects, modelObject{Key: markerKey, Data: markerData}), nil
}

func createImmutable(ctx context.Context, objects blob.Store, object modelObject) error {
	existing, err := createOrRead(ctx, objects, object)
	if err == nil && existing != nil {
		return fmt.Errorf("%w: immutable object %q contains hash %s, want %s", blob.ErrIntegrity, object.Key, hashHex(existing.Data), hashHex(object.Data))
	}
	return err
}

// createOrRead creates an immutable object, deciding an unknown outcome by
// reading the name back. A name that holds other bytes returns them: a
// content-addressed object is corrupt, a slot was won by another writer.
func createOrRead(ctx context.Context, objects blob.Store, object modelObject) (*blob.Object, error) {
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
			if bytes.Equal(existing.Data, object.Data) {
				return nil, nil
			}
			return &existing, nil
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
	if err := readMarker(ctx, objects, worldID); err != nil {
		return fmt.Errorf("validate existing world: %w", err)
	}
	checkpoint, err := newestCheckpoint(ctx, objects, worldID)
	if err == nil {
		_, err = loadCheckpoint(ctx, objects, checkpoint, defaultShardWorkers)
	}
	if err != nil {
		return fmt.Errorf("validate existing world: %w", err)
	}
	return nil
}
