package changefeed

import (
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// DocumentEvent is the hint for doc as just committed at path.
func DocumentEvent(path string, doc *storefmt.Document, op string) Event {
	return Event{
		Path:    storefmt.CanonicalPath(path),
		Version: doc.Version,
		Hash:    storefmt.ContentHash(doc.Content),
		Op:      op,
		Agent:   doc.Metadata[protocol.MetaAgent],
		User:    doc.Metadata[protocol.MetaUser],
	}
}

// Block encodes the event as a WATCH block under epoch.
func (ev Event) Block(epoch string) protocol.WatchBlock {
	return protocol.WatchEvent{
		Cursor:  protocol.Cursor{Epoch: epoch, Seq: ev.Seq},
		Path:    ev.Path,
		Version: ev.Version,
		Hash:    ev.Hash,
		Op:      ev.Op,
		Agent:   ev.Agent,
		User:    ev.User,
	}.Block()
}

// EventOf is the event a decoded WATCH block carries; the epoch is the
// caller's to check.
func EventOf(wire protocol.WatchEvent) Event {
	return Event{Seq: wire.Cursor.Seq, Path: wire.Path, Version: wire.Version, Hash: wire.Hash, Op: wire.Op, Agent: wire.Agent, User: wire.User}
}

// ArchiveOp is what an archive change reports: an unarchive shows the
// document again at its current version, which a watcher refetches like a
// publish.
func ArchiveOp(archived bool) string {
	if archived {
		return protocol.OpArchive
	}
	return protocol.OpPublish
}
