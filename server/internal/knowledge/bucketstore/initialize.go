package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// Initialize creates the deterministic empty object graph and create-only head.
func Initialize(ctx context.Context, objects blob.Store, worldID string) error {
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

	_, err := objects.Get(ctx, headObjectKey)
	switch {
	case err == nil:
		return validateExistingWorld(ctx, objects, worldID)
	case !errors.Is(err, blob.ErrNotFound):
		return fmt.Errorf("initialize bucket store: check head: %w", err)
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
	head := model[len(model)-1]
	if err := createGenesisHead(ctx, objects, worldID, head); err != nil {
		return fmt.Errorf("initialize bucket store: %w", err)
	}
	return nil
}

func buildGenesis(worldID string) ([]modelObject, error) {
	objects := make([]modelObject, 0, shardCount+2)
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
	head := headObject{
		Schema:   schemaVersion,
		WorldID:  worldID,
		Sequence: 1,
		Root:     rootRef,
		Receipts: make([]operationReceipt, 0),
	}
	if err := validateHeadObject(&head); err != nil {
		return nil, fmt.Errorf("build head: %w", err)
	}
	headData, err := marshalImmutable(head)
	if err != nil {
		return nil, fmt.Errorf("build head: %w", err)
	}
	return append(objects, modelObject{Key: headObjectKey, Data: headData}), nil
}

func createImmutable(ctx context.Context, objects blob.Store, object modelObject) error {
	var lastErr error
	for attempt := range maximumCreateAttempts {
		_, err := objects.Create(ctx, object.Key, object.Data)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, blob.ErrPrecondition) && !retryableObjectError(err) {
			return fmt.Errorf("create immutable object %q: %w", object.Key, err)
		}

		existing, getErr := objects.Get(ctx, object.Key)
		if getErr == nil {
			return verifyExistingImmutable(object, &existing)
		}
		lastErr = errors.Join(lastErr, getErr)
		if !errors.Is(getErr, blob.ErrNotFound) && !retryableObjectError(getErr) {
			return fmt.Errorf("reconcile immutable object %q: %w", object.Key, lastErr)
		}
		if attempt == maximumCreateAttempts-1 {
			break
		}
		if err := waitForRetry(ctx, createRetryDelay(attempt)); err != nil {
			return fmt.Errorf("wait to retry immutable object %q: %w", object.Key, errors.Join(lastErr, err))
		}
	}
	return fmt.Errorf("create immutable object %q retries exhausted: %w", object.Key, lastErr)
}

func verifyExistingImmutable(object modelObject, existing *blob.Object) error {
	if err := validateReadObject(object.Key, existing); err != nil {
		return fmt.Errorf("reconcile immutable object %q: %w", object.Key, err)
	}
	if !bytes.Equal(existing.Data, object.Data) {
		return fmt.Errorf("%w: immutable object %q contains hash %s, want %s", blob.ErrIntegrity, object.Key, hashHex(existing.Data), hashHex(object.Data))
	}
	return nil
}

func createGenesisHead(ctx context.Context, objects blob.Store, worldID string, head modelObject) error {
	var lastErr error
	for attempt := range maximumCreateAttempts {
		_, err := objects.Create(ctx, head.Key, head.Data)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, blob.ErrPrecondition) && !retryableObjectError(err) {
			return fmt.Errorf("create head: %w", err)
		}

		openErr := validateExistingWorld(ctx, objects, worldID)
		if openErr == nil {
			return nil
		}
		lastErr = errors.Join(lastErr, openErr)
		if !errors.Is(openErr, blob.ErrNotFound) && !retryableObjectError(openErr) {
			return fmt.Errorf("reconcile head create: %w", lastErr)
		}
		if attempt == maximumCreateAttempts-1 {
			break
		}
		if err := waitForRetry(ctx, createRetryDelay(attempt)); err != nil {
			return fmt.Errorf("wait to retry head create: %w", errors.Join(lastErr, err))
		}
	}
	return fmt.Errorf("create head retries exhausted: %w", lastErr)
}

func validateExistingWorld(ctx context.Context, objects blob.Store, worldID string) error {
	if _, err := Open(ctx, objects, Options{WorldID: worldID}); err != nil {
		return fmt.Errorf("validate existing world: %w", err)
	}
	return nil
}
