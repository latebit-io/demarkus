package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// slotRead is a verified slot and the hash the next slot names as Prev.
type slotRead struct {
	slot *slotObject
	hash string
}

// readSlot reads and verifies the slot named by first; a missing slot is
// blob.ErrNotFound, the log's tip.
func readSlot(ctx context.Context, objects blob.Store, worldID string, first int64) (slotRead, error) {
	object, err := objects.Get(ctx, slotKey(first))
	if err != nil {
		return slotRead{}, fmt.Errorf("read %q: %w", slotKey(first), err)
	}
	return parseSlot(&object, worldID, first)
}

// parseSlot verifies an object read at the slot name for first.
func parseSlot(object *blob.Object, worldID string, first int64) (slotRead, error) {
	slot, err := decodeValidated(slotKey(first), object, func(slot *slotObject) error {
		if err := validateSlot(slot, first); err != nil {
			return err
		}
		if slot.WorldID != worldID {
			return fmt.Errorf("slot belongs to world %q", slot.WorldID)
		}
		return nil
	})
	if err != nil {
		return slotRead{}, err
	}
	return slotRead{slot: &slot, hash: hashHex(object.Data)}, nil
}

// applySlot advances a derived snapshot by one slot. Paths whose body
// changed while live are added to reindex, for their section index.
func (s *snapshot) applySlot(read slotRead, reindex map[string]struct{}) error {
	slot := read.slot
	if slot.First != s.Sequence+1 {
		return fmt.Errorf("%w: slot %d follows sequence %d", blob.ErrIntegrity, slot.First, s.Sequence)
	}
	if slot.Prev != s.Tip {
		return fmt.Errorf("%w: slot %d names predecessor %q, the log's tip is %q", blob.ErrIntegrity, slot.First, slot.Prev, s.Tip)
	}
	for index := range slot.Entries {
		entry := &slot.Entries[index]
		if err := s.applyEntry(entry, reindex); err != nil {
			return fmt.Errorf("%w: sequence %d %s: %v", blob.ErrIntegrity, slot.First+int64(index), entry.Path, err)
		}
	}
	s.Sequence, s.Tip = slot.last(), read.hash
	return nil
}

// applyEntry installs one change, refusing what no writer could commit from
// this state. A live path left without sections is added to reindex.
func (s *snapshot) applyEntry(entry *slotEntry, reindex map[string]struct{}) error {
	old := s.path(entry.Path)
	state, err := s.entryState(old, entry)
	if err != nil {
		return err
	}
	if !state.Archived && state.Sections == nil {
		reindex[entry.Path] = struct{}{}
	}
	s.put(old, state)
	return nil
}

// entryState is the document after entry, from old (nil for a new path). It
// keeps old's sections only while the body is unchanged and live.
func (s *snapshot) entryState(old *pathState, entry *slotEntry) (*pathState, error) {
	modified, err := parseTimestamp(entry.Modified)
	if err != nil {
		return nil, err
	}
	state := &pathState{
		Path: entry.Path, Current: entry.Current, First: entry.First, Archived: entry.Archived,
		BodyHash: entry.BodyHash, Modified: modified,
	}
	if entry.Version == nil {
		if old == nil || old.Current != entry.Current || old.BodyHash != entry.BodyHash || !old.Modified.Equal(modified) || old.Archived == entry.Archived {
			return nil, errors.New("archive transition does not follow the document's state")
		}
		state.Entry, state.Base, state.Recent = old.Entry, old.Base, retainFrom(old.Recent, entry.First)
		return state, nil
	}
	switch {
	case old == nil:
		if entry.Current != 1 {
			return nil, fmt.Errorf("new document starts at version %d", entry.Current)
		}
		if err := validateNewPathTopology(s, entry.Path); err != nil {
			return nil, err
		}
	case old.Archived:
		return nil, errors.New("write to an archived document")
	case entry.Current != old.Current+1:
		return nil, fmt.Errorf("version %d follows %d", entry.Current, old.Current)
	case entry.First < old.First:
		return nil, fmt.Errorf("first retained version moves back from %d to %d", old.First, entry.First)
	default:
		state.Base, state.Recent = old.Base, retainFrom(old.Recent, entry.First)
		if old.BodyHash == entry.BodyHash {
			state.Sections = old.Sections
		}
	}
	if state.Entry, err = catalogEntry(entry.Catalog); err != nil {
		return nil, err
	}
	// Clipped, so the append never writes into an array another snapshot holds.
	state.Recent = append(slices.Clip(state.Recent), retainedVersion{entry: *entry.Version, modified: modified})
	return state, nil
}

// retainFrom drops the versions retention pruned below first.
func retainFrom(versions []retainedVersion, first int) []retainedVersion {
	index := 0
	for index < len(versions) && versions[index].entry.Version < first {
		index++
	}
	return versions[index:]
}

// catalogEntry is the prepared catalog entry a record describes.
func catalogEntry(record *catalogRecord) (*catalog.Entry, error) {
	importance, err := parseCanonicalImportance(record.Importance)
	if err != nil {
		return nil, err
	}
	modified, err := parseTimestamp(record.Modified)
	if err != nil {
		return nil, err
	}
	entry := &catalog.Entry{
		Path:       record.Path,
		Tags:       record.Tags,
		Importance: importance,
		Title:      record.Title,
		Modified:   modified,
		Metadata:   record.Metadata,
	}
	entry.Prepare()
	return entry, nil
}
