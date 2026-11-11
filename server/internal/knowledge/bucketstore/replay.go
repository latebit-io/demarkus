package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/latebit-io/demarkus/server/blob"
)

// applyError is a slot that failed to apply. The snapshot may then hold part
// of it, so nothing built on it is served.
type applyError struct{ error }

func (err *applyError) Unwrap() error { return err.error }

// replayOptions names the world, the read parallelism, and who hears of each
// applied slot. A positive until is the last sequence applied; stop, asked
// after each slot, ends the replay early; a replay short of reach fails.
type replayOptions struct {
	worldID string
	workers int
	onSlot  func(slot *slotObject)
	until   int64
	stop    func(slot *slotObject) bool
	reach   reach
}

// replayChunk is how many slots a replay reads ahead of applying them.
const replayChunk = 256

// replay applies every slot after loaded's sequence, a page of names at a
// time, reading the slots in parallel a chunk at a time.
func replay(ctx context.Context, objects blob.Store, loaded *snapshot, options replayOptions) error {
	if err := applyLog(ctx, objects, loaded, options); err != nil {
		return err
	}
	return options.reach.reached(loaded)
}

func applyLog(ctx context.Context, objects blob.Store, loaded *snapshot, options replayOptions) error {
	for page, err := range sequencedPages(ctx, objects, logPrefix, loaded.Sequence) {
		if err != nil {
			return err
		}
		listed := len(page)
		if options.until > 0 {
			page = slices.DeleteFunc(page, func(slot sequenced) bool { return slot.sequence > options.until })
		}
		for chunk := range slices.Chunk(page, replayChunk) {
			slots, err := readSlots(ctx, objects, options, chunk)
			if err != nil {
				return err
			}
			for _, read := range slots {
				if err := loaded.applySlot(read); err != nil {
					return &applyError{fmt.Errorf("replay: %w", err)}
				}
				if options.onSlot != nil {
					options.onSlot(read.slot)
				}
				if options.stop != nil && options.stop(read.slot) {
					return nil
				}
			}
		}
		if len(page) < listed {
			return nil
		}
	}
	return nil
}

// readSlots reads the listed slots in parallel; a listed slot that is
// missing is an integrity failure.
func readSlots(ctx context.Context, objects blob.Store, options replayOptions, listed []sequenced) ([]slotRead, error) {
	slots := make([]slotRead, len(listed))
	err := runParallel(ctx, options.workers, indexes(len(listed)), func(ctx context.Context, index int) error {
		read, err := readSlot(ctx, objects, options.worldID, listed[index].sequence)
		if errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("%w: listed slot %d is missing: %w", blob.ErrIntegrity, listed[index].sequence, err)
		}
		slots[index] = read
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	return slots, nil
}

// sequenced is a listed object named by its sequence.
type sequenced struct {
	blob.Attributes
	sequence int64
}

// sequencedPages yields, a page at a time, the objects a checkpoint or slot
// listing names after the given sequence; an error ends it.
func sequencedPages(ctx context.Context, objects blob.Store, prefix string, after int64) iter.Seq2[[]sequenced, error] {
	return func(yield func([]sequenced, error) bool) {
		startAfter := ""
		if after > 0 {
			startAfter = fmt.Sprintf("%s%016x.json", prefix, after)
		}
		for page, err := range attributePages(ctx, objects, prefix, startAfter) {
			if err != nil {
				yield(nil, err)
				return
			}
			named := make([]sequenced, len(page))
			for index, attributes := range page {
				sequence, ok := sequenceOfKey(attributes.Key, prefix)
				if !ok {
					yield(nil, fmt.Errorf("%w: %q is not a sequence name", blob.ErrIntegrity, attributes.Key))
					return
				}
				named[index] = sequenced{Attributes: attributes, sequence: sequence}
			}
			if !yield(named, nil) {
				return
			}
		}
	}
}

// attributePages lists prefix after startAfter, one non-empty page at a time.
func attributePages(ctx context.Context, objects blob.Store, prefix, startAfter string) iter.Seq2[[]blob.Attributes, error] {
	return func(yield func([]blob.Attributes, error) bool) {
		cursor := ""
		for {
			page, err := objects.List(ctx, prefix, startAfter, cursor)
			if err != nil {
				yield(nil, fmt.Errorf("list %q: %w", prefix, err))
				return
			}
			if len(page.Objects) > 0 && !yield(page.Objects, nil) || page.NextCursor == "" {
				return
			}
			cursor = page.NextCursor
		}
	}
}
