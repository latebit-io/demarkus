// Package hub edits link pages: a hub names its children once, links plus
// one line each. Journal and archive share these edits so a hub is kept
// current the same way from every surface.
package hub

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
)

// Title is the hub's name: its H1, else its title metadata, else fallback.
func Title(body string, meta map[string]string, fallback string) string {
	if t := strings.TrimSpace(links.ExtractTitle(body)); t != "" {
		return t
	}
	if t := strings.TrimSpace(meta["title"]); t != "" {
		return t
	}
	return fallback
}

// Link is one child a hub must reach: the directory or document, the bullet
// that links it (under Section when that heading exists), and what to
// publish when the hub does not exist yet.
type Link struct {
	Target     string // directory with its slash, or a document path
	Bullet     string
	Section    string // "## Journal"; "" appends at the end
	CreateBody string
	CreateMeta map[string]string
	Stamp      map[string]string // the surface's identity keys, on every write
}

// EnsureLink makes the hub reach the target: no write when it already does,
// else the bullet added (or the hub created) at the head's version, one retry
// on conflict. Head may be a fetch already in hand; nil fetches.
func EnsureLink(ctx context.Context, doc *docwrite.Doc, head *docwrite.Head, link *Link) error {
	result, _, err := doc.Edit(ctx, head, func(h *docwrite.Head) (docwrite.Write, bool, error) {
		if h.Status == protocol.StatusNotFound {
			return docwrite.Write{Body: link.CreateBody, Metadata: stamped(link.CreateMeta, link.Stamp)}, true, nil
		}
		if Reaches(h.Body, doc.Path, link.Target) {
			return docwrite.Write{}, false, nil
		}
		return docwrite.Write{Body: WithBullet(h.Body, link.Section, link.Bullet), Metadata: stamped(docwrite.Carried(h.Metadata), link.Stamp)}, true, nil
	})
	switch {
	case err != nil:
		return fmt.Errorf("hub %s not updated: %w", doc.Path, err)
	case result.Response.Status == protocol.StatusOK, result.Response.Status == protocol.StatusCreated, result.Response.Status == "":
		return nil
	}
	return fmt.Errorf("hub %s not updated: %s", doc.Path, result.Response.Status)
}

// Reaches reports a link from the hub to target: the target itself or, for
// a directory, anything under it.
func Reaches(body, hubPath, target string) bool {
	for _, dest := range links.Extract(body) {
		resolved := links.ResolvePath(hubPath, dest)
		if resolved == "" {
			continue
		}
		if resolved == target || resolved == strings.TrimSuffix(target, "/") ||
			(strings.HasSuffix(target, "/") && strings.HasPrefix(resolved, target)) {
			return true
		}
	}
	return false
}

// WithBullet appends the bullet at the end of section's list when the
// heading exists, else as a new section at the end; no section: at the end.
func WithBullet(body, section, bullet string) string {
	body = strings.TrimRight(body, "\n") + "\n"
	if section == "" {
		return body + bullet + "\n"
	}
	headings := mdoutline.Headings(body)
	for i, h := range headings {
		if "## "+h.Text != section {
			continue
		}
		end := len(body)
		if i+1 < len(headings) {
			end = headings[i+1].Start
		}
		return strings.TrimRight(body[:end], "\n") + "\n" + bullet + "\n\n" + body[end:]
	}
	return body + "\n" + section + "\n\n" + bullet + "\n"
}

// stamped overlays the surface's identity on the publisher metadata.
func stamped(meta, stamp map[string]string) map[string]string {
	out := make(map[string]string, len(meta)+len(stamp))
	maps.Copy(out, meta)
	maps.Copy(out, stamp)
	return out
}
