package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// readSlot reads and verifies the slot named by first; a missing slot is
// blob.ErrNotFound, the log's tip. hash is what the next slot names as Prev.
func readSlot(ctx context.Context, objects blob.Store, worldID string, first int64) (slot *slotObject, hash string, err error) {
	read, data, err := getValidated(ctx, objects, slotKey(first), func(slot *slotObject) error {
		if err := validateSlot(slot, first); err != nil {
			return err
		}
		if slot.WorldID != worldID {
			return fmt.Errorf("slot belongs to world %q", slot.WorldID)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return &read, hashHex(data), nil
}

// applySlot advances a derived snapshot by one slot. Paths whose body
// changed while live are added to reindex, for their section index.
func (s *snapshot) applySlot(slot *slotObject, hash string, reindex map[string]struct{}) error {
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
	s.Sequence, s.Tip = slot.last(), hash
	return nil
}

// applyEntry installs one change, refusing what no writer could commit from
// this state.
func (s *snapshot) applyEntry(entry *slotEntry, reindex map[string]struct{}) error {
	modified, err := parseTimestamp(entry.Modified)
	if err != nil {
		return err
	}
	old := s.path(entry.Path)
	state := &pathState{
		Path: entry.Path, Current: entry.Current, First: entry.First, Archived: entry.Archived,
		BodyHash: entry.BodyHash, Modified: modified,
	}
	if entry.Version == nil {
		if old == nil || old.Current != entry.Current || old.BodyHash != entry.BodyHash || !old.Modified.Equal(modified) || old.Archived == entry.Archived {
			return errors.New("archive transition does not follow the document's state")
		}
		state.Entry, state.Base, state.Recent = old.Entry, old.Base, retainFrom(old.Recent, entry.First)
		if !entry.Archived {
			reindex[entry.Path] = struct{}{}
		}
		s.put(old, state)
		return nil
	}
	switch {
	case old == nil:
		if entry.Current != 1 {
			return fmt.Errorf("new document starts at version %d", entry.Current)
		}
		if err := validateNewPathTopology(s, entry.Path); err != nil {
			return err
		}
	case old.Archived:
		return errors.New("write to an archived document")
	case entry.Current != old.Current+1:
		return fmt.Errorf("version %d follows %d", entry.Current, old.Current)
	case entry.First < old.First:
		return fmt.Errorf("first retained version moves back from %d to %d", old.First, entry.First)
	default:
		state.Base, state.Recent = old.Base, retainFrom(old.Recent, entry.First)
		if old.BodyHash == entry.BodyHash {
			state.Sections = old.Sections
		}
	}
	if state.Entry, err = catalogEntry(entry.Catalog); err != nil {
		return err
	}
	// Clipped, so the append never writes into an array another snapshot holds.
	state.Recent = append(slices.Clip(state.Recent), retainedVersion{entry: *entry.Version, modified: modified})
	if state.Sections == nil {
		reindex[entry.Path] = struct{}{}
	}
	s.put(old, state)
	return nil
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
