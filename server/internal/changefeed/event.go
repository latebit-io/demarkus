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
		Agent:   doc.Metadata["agent"],
	}
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
