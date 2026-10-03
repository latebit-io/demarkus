package bucketstore

import (
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// Changes is the hub this store feeds, or nil when WATCH is off: under the
// world ID and log sequences, so cursors agree across replicas and restarts
// (backend.ChangeSource), with the slots as its backlog.
func (store *Store) Changes() *changefeed.Hub { return store.changes }

// skipTo starts the hub at a checkpoint: what came before it resumes from the
// backlog, or resyncs.
func (store *Store) skipTo(sequence int64) {
	if store.changes != nil {
		store.changes.Skip(hubSeq(sequence))
	}
}

// report publishes one applied slot's changes in log order, so watchers on
// this replica learn of every replica's commits under one sequence.
func (store *Store) report(slot appliedSlot) {
	if store.changes == nil {
		return
	}
	for _, event := range slot.events {
		store.changes.PublishAt(event)
	}
}

// appliedSlot is what an install keeps of a slot it applied: its changes,
// which name its paths, and the store that wrote it, never the decoded slot.
type appliedSlot struct {
	store  string
	events []changefeed.Event
}

func appliedOf(slot *slotObject) appliedSlot {
	return appliedSlot{store: slot.Store, events: slotEvents(slot)}
}

// slotEvents are the change hints a slot names.
func slotEvents(slot *slotObject) []changefeed.Event {
	events := make([]changefeed.Event, len(slot.Entries))
	for index := range slot.Entries {
		entry := &slot.Entries[index]
		events[index] = changefeed.Event{
			Seq: hubSeq(slot.First + int64(index)), Path: entry.Path, Version: entry.Current,
			Hash: entry.BodyHash, Op: entry.Op, Agent: entry.Agent,
		}
	}
	return events
}

// hubSeq is a log sequence as a cursor sequence; sequences are validated
// positive, so the guard only satisfies the conversion check.
func hubSeq(sequence int64) uint64 {
	if sequence < 0 {
		return 0
	}
	return uint64(sequence)
}
