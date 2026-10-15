package marktools

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/docwrite/gate"
	"github.com/latebit-io/demarkus/client/hub"
	"github.com/latebit-io/demarkus/client/journal"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// linkOnPublish links a first version from the nearest ancestor hub that
// exists, unless one already names it: a hubs line names the edit; a hub
// not editable, or none at all, is a note. The publish stands either way.
func (t *Tools) linkOnPublish(ctx context.Context, at *landing, w *docwrite.Write, result *docwrite.Result) string {
	version, err := docwrite.Version(result.Response.Metadata)
	if err != nil || version != 1 || !linkable(at.target.Path) {
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
		added, err := hub.EnsureLink(ctx, doc, h, &hub.Link{Target: at.target.Path, Bullet: bulletFor(at.target.Path, w), Stamp: t.identityMeta(ctx)})
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

// linkable: a document a hub should name. Hubs, logs, the store's own
// .well-known pages and dated journal entries (reached through their
// directory) are not.
func linkable(p string) bool {
	return !gate.NavExempt(p) && !strings.HasPrefix(p, "/.well-known/") && !journal.IsEntry(path.Base(p))
}

// bulletFor is the hub's one line for the document: its title linked, then
// the description or the summary under the H1 when the hub rule allows it.
func bulletFor(p string, w *docwrite.Write) string {
	title := hub.Title(w.Body, w.Metadata, strings.TrimSuffix(path.Base(p), ".md"))
	_, summary := gate.Summary(w.Body)
	if d := w.Metadata["description"]; d != "" {
		summary = d
	}
	bullet := fmt.Sprintf("- [%s](%s)", linkText.Replace(title), p)
	if s := gate.BulletSummary(title, summary); s != "" {
		bullet += ": " + s
	}
	return bullet
}

// linkText keeps a title inside its link: a bracket or backslash in it would
// end the link text early.
var linkText = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`)

// unlinked words the finding when no ancestor hub exists, with the fix.
func unlinked(p string) string {
	nearest := hub.Ancestors(p)[0]
	fix := "publish " + nearest + " with a bullet for it"
	if nearest != "/index.md" {
		fix += ", or add one to /index.md"
	}
	return "unlinked: no hub reaches " + p + "; " + fix
}
