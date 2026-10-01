package bucketstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/server/blob"
)

// EnsureWorld creates or finishes the world's genesis and reports whether it
// wrote it. Genesis objects without a head are a racing or interrupted replica;
// any other object without a head is refused as a misconfigured bucket URL.
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
	foreign, err := foreignObject(ctx, objects, worldID)
	if err != nil {
		return false, err
	}
	if foreign != "" {
		return false, fmt.Errorf("ensure world: %w: bucket holds %q but no world head",
			blob.ErrPrecondition, foreign)
	}
	if err := Initialize(ctx, objects, worldID); err != nil {
		return false, err
	}
	return true, nil
}

// foreignObject returns the first key that is not part of worldID's genesis,
// or "" when the bucket holds nothing else.
func foreignObject(ctx context.Context, objects blob.Store, worldID string) (string, error) {
	model, err := buildGenesis(worldID)
	if err != nil {
		return "", fmt.Errorf("ensure world: %w", err)
	}
	genesis := make(map[string]bool, len(model))
	for _, object := range model {
		genesis[object.Key] = true
	}
	cursor := ""
	for {
		listed, err := objects.List(ctx, "", cursor)
		if err != nil {
			return "", fmt.Errorf("ensure world: list bucket: %w", err)
		}
		for _, object := range listed.Objects {
			if !genesis[object.Key] {
				return object.Key, nil
			}
		}
		if listed.NextCursor == "" {
			return "", nil
		}
		cursor = listed.NextCursor
	}
}
