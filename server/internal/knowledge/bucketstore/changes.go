package bucketstore

import (
	"context"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// pathState is what a change hint is derived from: a path's current version,
// archive flag and body hash.
type pathState struct {
	Current  int
	Archived bool
	BodyHash string
}

// baseline records snap as already reported: what the store opened on is
// nobody's change.
func (store *Store) baseline(snap *snapshot) {
	if store.changes == nil {
		return
	}
	store.seenMu.Lock()
	defer store.seenMu.Unlock()
	for index := range shardCount {
		for i := range snap.Shards[index].Entries {
			entry := &snap.Shards[index].Entries[i]
			store.seen[entry.Path] = pathState{Current: entry.Current, Archived: entry.Archived, BodyHash: entry.BodyHash}
		}
	}
	store.mark(snap)
}

// mark records snap as the last reported snapshot.
func (store *Store) mark(snap *snapshot) {
	copy(store.reportedShards[:], snap.Root.Shards)
	store.reportedSeq = snap.Head.Sequence
}

// report publishes a hint for every path whose state in snap differs from
// the last reported, scanning only the shards whose ref moved. A peer's
// write gets an op inferred from the state (an append looks like a publish).
func (store *Store) report(snap *snapshot) {
	if store.changes == nil {
		return
	}
	store.seenMu.Lock()
	defer store.seenMu.Unlock()
	// A refresh that raced a local commit can hand over an older snapshot;
	// reporting it would announce versions already superseded.
	if snap.Head.Sequence < store.reportedSeq {
		return
	}
	for index := range shardCount {
		if snap.Root.Shards[index] == store.reportedShards[index] {
			continue
		}
		for i := range snap.Shards[index].Entries {
			store.reportEntry(&snap.Shards[index].Entries[i])
		}
	}
	store.mark(snap)
}

// reportEntry publishes entry when its state moved.
func (store *Store) reportEntry(entry *shardEntry) {
	state := pathState{Current: entry.Current, Archived: entry.Archived, BodyHash: entry.BodyHash}
	previous, known := store.seen[entry.Path]
	if known && previous == state {
		return
	}
	store.seen[entry.Path] = state
	store.changes.Publish(changefeed.Event{Path: entry.Path, Version: state.Current, Hash: state.BodyHash, Op: inferOp(previous, known, state)})
}

func inferOp(previous pathState, known bool, state pathState) string {
	if known && state.Current == previous.Current && state.Archived != previous.Archived && state.Archived {
		return protocol.OpArchive
	}
	return protocol.OpPublish
}

// reportLocal publishes the hint for this replica's own commit with the op
// and agent it knows, and records the state so a poll does not repeat it.
// Called under the commit token, so hints and the state follow commit order.
func (store *Store) reportLocal(result mutationResult) {
	if store.changes == nil || !result.Changed {
		return
	}
	doc := result.Document
	store.seenMu.Lock()
	defer store.seenMu.Unlock()
	store.seen[result.Path] = pathState{Current: doc.Version, Archived: doc.Archived, BodyHash: result.BodyHash}
	// A poll holding a snapshot from before this commit must not announce
	// the path's older state over this one.
	store.reportedSeq = max(store.reportedSeq, result.Sequence)
	store.changes.Publish(changefeed.Event{Path: result.Path, Version: doc.Version, Hash: result.BodyHash, Op: result.Op, Agent: doc.Metadata["agent"]})
}

// Poll refreshes the snapshot from the bucket and reports what peers wrote
// since the last report: the backstop that lets a watch on one replica see
// another replica's writes.
func (store *Store) Poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	snap, err := store.refreshSnapshot(ctx)
	if err != nil {
		return err
	}
	store.report(snap)
	return nil
}
