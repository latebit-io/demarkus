package changefeed

import (
	"context"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// Emit wraps store so every committed write publishes a hint to hub. Reads
// pass through. It goes outermost, so it sees what was stored.
func Emit(store backend.Store, hub *Hub) backend.Store {
	return &emitter{Store: store, hub: hub}
}

type emitter struct {
	backend.Store
	hub *Hub
}

func (e *emitter) Publish(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	doc, err := e.Store.Publish(ctx, req)
	if err == nil {
		e.hub.Publish(eventFor(req.Path, doc, protocol.OpPublish))
	}
	return doc, err
}

func (e *emitter) Append(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	doc, err := e.Store.Append(ctx, req)
	if err == nil {
		e.hub.Publish(eventFor(req.Path, doc, protocol.OpAppend))
	}
	return doc, err
}

// SetArchived emits archive for an archive and publish for an unarchive: the
// document is visible again at its current version, which is what a watcher
// must refetch.
func (e *emitter) SetArchived(ctx context.Context, req backend.ArchiveRequest) (backend.ArchiveResult, error) {
	result, err := e.Store.SetArchived(ctx, req)
	if err == nil && result.Changed {
		op := protocol.OpPublish
		if req.Archived {
			op = protocol.OpArchive
		}
		e.hub.Publish(eventFor(req.Path, result.Document, op))
	}
	return result, err
}

func eventFor(path string, doc *storefmt.Document, op string) Event {
	return Event{
		Path:    storefmt.CanonicalPath(path),
		Version: doc.Version,
		Hash:    storefmt.ContentHash(doc.Content),
		Op:      op,
		Agent:   doc.Metadata["agent"],
	}
}
