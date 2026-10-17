package marktools

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/protocol"
)

// linkOnPublish links a first version from the nearest ancestor hub that
// exists, unless one already names it: a hubs line names the edit; a hub
// not editable, or none at all, is a note. The publish stands either way.
func (t *Tools) linkOnPublish(ctx context.Context, at *landing, w *docwrite.Write, result *docwrite.Result) string {
	version, err := docwrite.Version(result.Response.Metadata)
	if err != nil || version != 1 || !hub.Linkable(at.target.Path) {
		return ""
	}
	for _, hubPath := range hub.Ancestors(at.target.Path) {
		// The hub shares the document's tokens: one authorization, one store.
		doc := &docwrite.Doc{Backend: t.backend, Host: at.target.Host, Path: hubPath, ReadToken: at.doc.ReadToken, Write: at.doc.Write}
		h, err := doc.Head(ctx)
		if err != nil {
			return mcpfmt.Note(fmt.Sprintf("hub %s not updated: %v", hubPath, err))
		}
		switch h.Status {
		case protocol.StatusNotFound, protocol.StatusArchived:
			continue // the next ancestor may name it
		case protocol.StatusOK:
		default:
			return mcpfmt.Note(fmt.Sprintf("hub %s not updated: %s", hubPath, h.Status))
		}
		added, err := hub.EnsureLink(ctx, doc, h, &hub.Link{Target: at.target.Path, Bullet: hub.Bullet(at.target.Path, w.Body, w.Metadata), Stamp: t.identityMeta(ctx)})
		switch {
		case err != nil:
			return mcpfmt.Note(err.Error())
		case added:
			return "hubs:\n- " + hubPath + ": bullet added\n"
		}
		return ""
	}
	return mcpfmt.Note(unlinked(at.target.Path))
}

// unlinked words the finding when no ancestor hub exists, with the fix.
func unlinked(p string) string {
	nearest := hub.Ancestors(p)[0]
	fix := "publish " + nearest + " with a bullet for it"
	if nearest != "/index.md" {
		fix += ", or add one to /index.md"
	}
	return "unlinked: no hub reaches " + p + "; " + fix
}
