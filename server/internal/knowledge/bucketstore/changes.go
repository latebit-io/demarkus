package bucketstore

import (
	"context"

	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// report publishes the head's receipts the hub has not seen, in head order,
// so watchers on this replica learn of every replica's commits under one
// sequence. Called with refreshMu held wherever a snapshot is installed.
func (store *Store) report(snap *snapshot) {
	hub := store.changes
	if hub == nil {
		return
	}
	head := &snap.Head
	from := hub.Head().Seq
	// Receipts cover (covered, head.Sequence]; anything older is a gap the
	// watchers rebuild from.
	covered := hubSeq(head.Sequence - int64(len(head.Receipts)))
	if from < covered {
		hub.Skip(covered)
		from = covered
	}
	for i := range head.Receipts {
		receipt := &head.Receipts[i]
		seq := hubSeq(receipt.Sequence)
		if seq <= from {
			continue
		}
		if receipt.Path == "" {
			// Written before receipts named their change: no hint to give.
			hub.Skip(seq)
			continue
		}
		hub.PublishAt(changefeed.Event{Seq: seq, Path: receipt.Path, Version: receipt.Version, Hash: receipt.Hash, Op: receipt.Op, Agent: receipt.Agent})
	}
}

// hubSeq is a head sequence as a cursor sequence; heads are validated
// positive, so the guard only satisfies the conversion check.
func hubSeq(sequence int64) uint64 {
	if sequence < 0 {
		return 0
	}
	return uint64(sequence)
}

// Poll refreshes the snapshot from the bucket, reporting what peers wrote:
// the backstop that lets a watch on one replica see another replica's
// writes when no hint arrived.
func (store *Store) Poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	_, err := store.refreshSnapshot(ctx)
	return err
}

// HeadSequence is the sequence of the snapshot this replica serves.
func (store *Store) HeadSequence() int64 {
	if snap := store.snapshot.Load(); snap != nil {
		return snap.Head.Sequence
	}
	return 0
}
