package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

// dropPlan is the checkpoints to drop, at their listed generations, the
// root and shard keys the kept ones use, and the oldest kept's sequence.
type dropPlan struct {
	dropFence
	dropped    []sequenced
	kept       map[string]bool
	oldestKept int64
}

// dropFence is when a drop started and the grace it honors.
type dropFence struct {
	started time.Time
	grace   time.Duration
}

// expired reports something written at modified as past the grace.
func (fence dropFence) expired(modified time.Time) bool {
	return fence.started.Sub(modified) >= fence.grace
}

// planDrop lists the checkpoints and reads the kept ones' roots.
func (store *Store) planDrop(ctx context.Context) (dropPlan, error) {
	plan := dropPlan{dropFence: dropFence{started: store.now(), grace: store.checkpointGrace}, kept: make(map[string]bool)}
	var listed []sequenced
	for page, err := range sequencedPages(ctx, store.objects, checkpointPrefix, 0) {
		if err != nil {
			return dropPlan{}, err
		}
		listed = append(listed, page...)
	}
	var kept []int64
	for index := range listed {
		if index < len(listed)-checkpointsKept && plan.expired(listed[index+1].Modified) {
			plan.dropped = append(plan.dropped, listed[index])
			continue
		}
		if len(kept) == 0 {
			plan.oldestKept = listed[index].sequence
		}
		kept = append(kept, listed[index].sequence)
	}
	if len(plan.dropped) == 0 {
		return plan, nil
	}
	// Racing compactors may write sequences out of order, so every kept root
	// is read, not only those after the dropped ones.
	for _, sequence := range kept {
		root, err := store.checkpointRoot(ctx, sequence)
		if err != nil {
			return dropPlan{}, err
		}
		plan.kept[root.Key] = true
		for _, shard := range root.layout.Shards {
			plan.kept[shard.Key] = true
		}
	}
	return plan, nil
}

// checkpointRoot reads the root of the checkpoint at sequence.
func (store *Store) checkpointRoot(ctx context.Context, sequence int64) (rootRead, error) {
	checkpoint, err := readCheckpoint(ctx, store.objects, store.worldID, sequence)
	if err != nil {
		return rootRead{}, err
	}
	return loadRoot(ctx, store.objects, checkpoint)
}

// applyDrop deletes each dropped checkpoint's own shards, then its root, then
// the checkpoint, so a drop cut short is found and finished by the next.
// Segments are swept by the checkpoint that supersedes them.
func (store *Store) applyDrop(ctx context.Context, plan dropPlan) error {
	var failures []error
	for _, checkpoint := range plan.dropped {
		if err := store.drop(ctx, plan, checkpoint); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (store *Store) drop(ctx context.Context, plan dropPlan, checkpoint sequenced) error {
	root, err := store.checkpointRoot(ctx, checkpoint.sequence)
	switch {
	case err == nil:
		var shards []string
		for _, shard := range root.layout.Shards {
			if !plan.kept[shard.Key] {
				shards = append(shards, shard.Key)
			}
		}
		err = runParallel(ctx, store.shardWorkers, shards, func(ctx context.Context, key string) error {
			return store.deleteUnused(ctx, key, plan.dropFence)
		})
		if err == nil && !plan.kept[root.Key] {
			err = store.deleteUnused(ctx, root.Key, plan.dropFence)
		}
		if err != nil {
			return fmt.Errorf("drop %q: %w", checkpoint.Key, err)
		}
	case !errors.Is(err, blob.ErrNotFound):
		return fmt.Errorf("drop %q: %w", checkpoint.Key, err)
	}
	// A missing root was deleted by a drop cut short, after its shards.
	return deleteAt(ctx, store.objects, checkpoint.Attributes)
}

// dropSlots deletes, oldest first a page at a time, the slots the oldest kept
// checkpoint covers that were slotRetention old when the drop began, each at
// its listed generation; a resume from before them resyncs.
func (store *Store) dropSlots(ctx context.Context, plan dropPlan) error {
	fence := dropFence{started: plan.started, grace: slotRetention}
	var previous *sequenced // a slot ends where the next one starts
	for page, err := range sequencedPages(ctx, store.objects, logPrefix, 0) {
		if err != nil {
			return err
		}
		var expired []blob.Attributes
		for index := range page {
			slot := &page[index]
			if previous != nil {
				if slot.sequence-1 > plan.oldestKept || !fence.expired(previous.Modified) {
					return store.deleteListed(ctx, expired)
				}
				expired = append(expired, previous.Attributes)
			}
			previous = slot
		}
		if err := store.deleteListed(ctx, expired); err != nil {
			return err
		}
	}
	return nil
}

// deleteListed deletes listed objects in parallel, each at its listed generation.
func (store *Store) deleteListed(ctx context.Context, listed []blob.Attributes) error {
	return runParallel(ctx, store.shardWorkers, listed, func(ctx context.Context, attributes blob.Attributes) error {
		return deleteAt(ctx, store.objects, attributes)
	})
}

// deleteUnused deletes a shard or root no kept checkpoint uses, unless it was
// written or reused within the grace before the drop's start.
func (store *Store) deleteUnused(ctx context.Context, key string, fence dropFence) error {
	attributes, err := store.objects.Head(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("head %q: %w", key, err)
	}
	if !fence.expired(attributes.Modified) {
		return nil
	}
	return deleteAt(ctx, store.objects, attributes)
}

// deleteAt deletes an object at the generation read; one rewritten or gone
// since is left as it is.
func deleteAt(ctx context.Context, objects blob.Store, attributes blob.Attributes) error {
	err := objects.Delete(ctx, attributes.Key, attributes.Generation)
	if err != nil && !errors.Is(err, blob.ErrNotFound) && !errors.Is(err, blob.ErrPrecondition) {
		return fmt.Errorf("delete %q: %w", attributes.Key, err)
	}
	return nil
}
