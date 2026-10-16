package hub

import (
	"fmt"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite/gate"
	"github.com/latebit-io/demarkus/protocol/layout"
)

// Linkable reports a document a hub should name. Hubs, logs, the store's
// own .well-known pages and dated journal entries (reached through their
// directory) are not.
func Linkable(p string) bool {
	return !gate.NavExempt(p) && !strings.HasPrefix(p, "/.well-known/") && !layout.IsJournalEntry(path.Base(p))
}

// Bullet is the hub's one line for the document: its title linked, then
// the description or the summary under the H1 when the hub rule allows it.
func Bullet(p, body string, meta map[string]string) string {
	title := Title(body, meta, strings.TrimSuffix(path.Base(p), ".md"))
	_, summary := gate.Summary(body)
	if d := meta["description"]; d != "" {
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
