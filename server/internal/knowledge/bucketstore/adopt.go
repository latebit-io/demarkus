package bucketstore

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/latebit-io/demarkus/server/blob"
)

// layout is the checkpoint the store last rebased on.
func (store *Store) layout() *checkpointBase {
	if adopted := store.adoption.Load(); adopted != nil {
		return adopted.checkpoint
	}
	return store.served.Load().snap.Checkpoint
}

// adoptPeerCheckpoint rebases on a checkpoint another store wrote, reading
// only the shards that differ from the one this store rests on.
func (store *Store) adoptPeerCheckpoint(ctx context.Context, sequence int64) error {
	checkpoint, err := readCheckpoint(ctx, store.objects, store.worldID, sequence)
	if err != nil {
		return fmt.Errorf("adopt: %w", err)
	}
	root, err := loadRoot(ctx, store.objects, checkpoint)
	if err != nil {
		return fmt.Errorf("adopt checkpoint %d: %w", sequence, err)
	}
	current := store.layout()
	var changed []int
	for index, ref := range root.layout.Shards {
		if current.Bits != root.layout.Bits || current.Shards[index] != ref {
			changed = append(changed, index)
		}
	}
	// A shard at a time per worker, and only bases: entries the served
	// snapshot rests on already are left out, so the adoption holds what changed.
	reader := shardReader{objects: store.objects, layout: root.layout, workers: store.shardWorkers}
	served := store.served.Load().snap
	var mu sync.Mutex
	entries := make(map[string]*baseEntry)
	err = runParallel(ctx, store.shardWorkers, changed, func(ctx context.Context, index int) error {
		shard, err := reader.shard(ctx, index)
		if err != nil {
			return fmt.Errorf("load shard %s: %w", root.layout.Shards[index].Shard, err)
		}
		for entryIndex := range shard.Entries {
			entry := &shard.Entries[entryIndex]
			base, err := foldedBase(entry)
			if err != nil {
				return fmt.Errorf("%w: checkpoint path %q: %v", blob.ErrIntegrity, entry.Path, err)
			}
			if old := served.path(entry.Path); old == nil || !sameBase(old.Base, base) {
				mu.Lock()
				entries[entry.Path] = base
				mu.Unlock()
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("adopt checkpoint %d: %w", sequence, err)
	}
	a := &adoption{checkpoint: root.layout, entries: entries}
	store.installAdoption(a)
	store.advanceBase(ctx, current.Sequence, a)
	return nil
}

// adoption is a checkpoint the store's snapshots rebase on: its layout and
// the entry of every document that changed in it.
type adoption struct {
	checkpoint *checkpointBase
	entries    map[string]*baseEntry
	// own is set when this store's create made the checkpoint, not a racer's.
	own bool
}

// rebaseAttempts is how often an adoption rebases the served snapshot
// outside installMu before holding it: a restart repeats the whole rebase,
// so a writer committing faster than one would never finish.
const rebaseAttempts = 3

// installAdoption makes a the newest checkpoint and rebases the served
// snapshot on it outside installMu, so no commit waits; an install meanwhile
// restarts it, the last attempt under the lock. Earlier snapshots rebase as installed.
func (store *Store) installAdoption(a *adoption) {
	for attempt := 1; ; attempt++ {
		locked := attempt >= rebaseAttempts
		if locked {
			store.installMu.Lock()
		}
		current := store.served.Load()
		snap := current.snap
		if store.rebasedPast(a, snap) {
			if locked {
				store.installMu.Unlock()
			}
			return
		}
		rebased := snap
		// A served snapshot behind the checkpoint rebases once it passes it.
		if snap.Sequence >= a.checkpoint.Sequence {
			if store.holdRebase != nil && !locked {
				store.holdRebase()
			}
			rebased = store.rebase(snap, a)
		}
		if !locked {
			store.installMu.Lock()
			if current = store.served.Load(); current.snap != snap {
				store.installMu.Unlock()
				continue
			}
			if store.rebasedPast(a, snap) {
				store.installMu.Unlock()
				return
			}
		}
		store.adoption.Store(a)
		store.served.Store(&served{snap: rebased, confirmed: current.confirmed})
		store.releaseAdoption(rebased)
		store.installMu.Unlock()
		return
	}
}

// rebasedPast reports whether the store already rests on a or a newer
// checkpoint, by its adoption or by a reload that moved the served snapshot.
func (store *Store) rebasedPast(a *adoption, snap *snapshot) bool {
	if current := store.adoption.Load(); current != nil && current.checkpoint.Sequence >= a.checkpoint.Sequence {
		return true
	}
	return snap.Checkpoint.Sequence >= a.checkpoint.Sequence
}

// releaseAdoption drops the adoption's entries once nothing built before it
// can still be installed: the served snapshot rests on it and the committer
// is not running. Called under installMu, which orders every install.
func (store *Store) releaseAdoption(served *snapshot) {
	a := store.adoption.Load()
	if a == nil || a.entries == nil || served.Checkpoint.Sequence < a.checkpoint.Sequence || store.commits.active() {
		return
	}
	store.adoption.Store(&adoption{checkpoint: a.checkpoint})
}

// rebased is s on the newest adopted checkpoint when s was built before it
// and holds it; otherwise s.
func (store *Store) rebased(s *snapshot) *snapshot {
	a := store.adoption.Load()
	if a == nil || s.Checkpoint.Sequence >= a.checkpoint.Sequence || s.Sequence < a.checkpoint.Sequence {
		return s
	}
	return store.rebase(s, a)
}

// rebase is s on a; a failure marks the store diverged and keeps s.
func (store *Store) rebase(s *snapshot, a *adoption) *snapshot {
	next := store.derive(s)
	if err := next.adopt(a); err != nil {
		// The compactor refuses a snapshot off the newest checkpoint, so the
		// next refresh starts the store again from it.
		store.diverged.Store(true)
		store.logger.Error("rebase on checkpoint failed; reloading from the newest checkpoint", "world", store.worldID, "checkpoint", a.checkpoint.Sequence, "error", err)
		return s
	}
	return next
}

// adopt rebases a derived snapshot at or past a's checkpoint on it, dropping
// the versions the checkpoint now holds.
func (s *snapshot) adopt(a *adoption) error {
	for path, base := range a.entries {
		old := s.path(path)
		if old == nil || old.Current < base.Current {
			return fmt.Errorf("%w: checkpoint %d holds %s ahead of the log", blob.ErrIntegrity, a.checkpoint.Sequence, path)
		}
		if sameBase(old.Base, base) {
			continue
		}
		state := *old
		state.Base, state.Recent = base, nil
		if index := slices.IndexFunc(old.Recent, func(version retainedVersion) bool { return version.entry.Version > base.Current }); index >= 0 {
			// Cloned: the old array holds the versions the checkpoint took.
			state.Recent = slices.Clone(old.Recent[index:])
		}
		s.put(old, &state)
	}
	s.Checkpoint = a.checkpoint
	return nil
}

func sameBase(a, b *baseEntry) bool {
	return a != nil && b != nil && slices.Equal(a.History, b.History) &&
		a.Current == b.Current && a.Archived == b.Archived && a.BodyHash == b.BodyHash && a.Modified.Equal(b.Modified)
}
