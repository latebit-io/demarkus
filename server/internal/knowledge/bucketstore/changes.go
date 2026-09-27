package bucketstore

import (
	"context"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// pathState is what a change hint is derived from: a path's current version,
// archive flag and body hash.
type pathState struct {
	Current  int
	Archived bool
	BodyHash string
}

// report publishes a hint for every path whose state in snap differs from
// the last reported, scanning only the shards whose ref moved. A peer's
// write gets an op inferred from the state (an append looks like a publish).
func (store *Store) report(snap *snapshot) {
	if store.changes == nil || snap == nil {
		return
	}
	store.seenMu.Lock()
	defer store.seenMu.Unlock()
	// A refresh that raced a local commit can hand over an older snapshot;
	// reporting it would announce versions already superseded.
	if store.baselined && snap.Head.Sequence < store.reportedSeq {
		return
	}
	for index := range shardCount {
		if store.baselined && snap.Root.Shards[index] == store.reportedShards[index] {
			continue
		}
		for i := range snap.Shards[index].Entries {
			store.reportEntry(&snap.Shards[index].Entries[i])
		}
	}
	copy(store.reportedShards[:], snap.Root.Shards)
	store.reportedSeq = snap.Head.Sequence
	store.baselined = true
}

// reportEntry publishes entry when its state moved; before the baseline it
// only records the state.
func (store *Store) reportEntry(entry *shardEntry) {
	state := pathState{Current: entry.Current, Archived: entry.Archived, BodyHash: entry.BodyHash}
	previous, known := store.seen[entry.Path]
	if known && previous == state {
		return
	}
	store.seen[entry.Path] = state
	if store.baselined {
		store.changes.Publish(changefeed.Event{Path: entry.Path, Version: state.Current, Hash: state.BodyHash, Op: inferOp(previous, known, state)})
	}
}

func inferOp(previous pathState, known bool, state pathState) string {
	if known && state.Current == previous.Current && state.Archived != previous.Archived && state.Archived {
		return protocol.OpArchive
	}
	return protocol.OpPublish
}

// reportLocal publishes the hint for this replica's own commit with the op
// and agent it knows, and records the state so a poll does not repeat it.
func (store *Store) reportLocal(op, reqPath string, result mutationResult) {
	if store.changes == nil || result.Document == nil {
		return
	}
	path, err := canonicalMutationPath(reqPath)
	if err != nil {
		store.logger.Warn("change hint dropped", "path", reqPath, "error", err)
		return
	}
	doc := result.Document
	state := pathState{Current: doc.Version, Archived: doc.Archived, BodyHash: storefmt.ContentHash(doc.Content)}
	store.seenMu.Lock()
	store.seen[path] = state
	// A poll holding a snapshot from before this commit must not announce
	// the path's older state over this one.
	store.reportedSeq = max(store.reportedSeq, result.Sequence)
	store.seenMu.Unlock()
	store.changes.Publish(changefeed.Event{Path: path, Version: doc.Version, Hash: state.BodyHash, Op: op, Agent: doc.Metadata["agent"]})
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
