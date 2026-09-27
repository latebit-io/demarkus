package bucketstore

import (
	"context"
	"sort"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// pathState is what a change hint is derived from: a path's current version,
// archive flag and body hash in a snapshot.
type pathState struct {
	Current  int
	Archived bool
	BodyHash string
}

// localWrite names the operation this replica just committed, so its hint
// carries the exact op and agent instead of ones inferred from the snapshot.
type localWrite struct {
	path, op, agent string
}

// report publishes a hint for every path whose state in snap differs from
// the last reported. A peer's write, seen only by a poll, gets an op inferred
// from the snapshot (an append looks like a publish); the first report is the baseline.
func (store *Store) report(snap *snapshot, local *localWrite) {
	if store.changes == nil || snap == nil {
		return
	}
	store.seenMu.Lock()
	defer store.seenMu.Unlock()
	// A refresh that raced a local commit can install an older snapshot
	// after it; reporting it would announce versions already superseded.
	if snap.Head.Sequence < store.reportedSeq {
		return
	}
	store.reportedSeq = snap.Head.Sequence
	paths := make([]string, 0, len(snap.Paths))
	for path := range snap.Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		entry := snap.Paths[path]
		state := pathState{Current: entry.Current, Archived: entry.Archived, BodyHash: entry.BodyHash}
		previous, known := store.seen[path]
		if known && previous == state {
			continue
		}
		store.seen[path] = state
		if !store.baselined {
			continue
		}
		event := changefeed.Event{Path: path, Version: state.Current, Hash: state.BodyHash, Op: inferOp(previous, known, state)}
		if local != nil && local.path == path {
			event.Op, event.Agent = local.op, local.agent
		}
		store.changes.Publish(event)
	}
	store.baselined = true
}

func inferOp(previous pathState, known bool, state pathState) string {
	if known && state.Current == previous.Current && state.Archived != previous.Archived && state.Archived {
		return protocol.OpArchive
	}
	return protocol.OpPublish
}

// reportLocal reports the snapshot after this replica's own commit with the
// operation it knows.
func (store *Store) reportLocal(op, reqPath, agent string) {
	if store.changes == nil {
		return
	}
	path, err := canonicalMutationPath(reqPath)
	if err != nil {
		return
	}
	store.report(store.snapshot.Load(), &localWrite{path: path, op: op, agent: agent})
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
	store.report(snap, nil)
	return nil
}
