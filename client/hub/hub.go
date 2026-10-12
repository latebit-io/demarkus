// Package hub edits link pages: a hub names its children once, links plus
// one line each. Journal and archive share these edits so a hub is kept
// current the same way from every surface.
package hub

import (
	"context"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
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

// Docs binds a surface's write contract to one path on the store.
type Docs func(docPath string) *docwrite.Doc

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
	return settled(doc, &result, err, protocol.StatusOK, protocol.StatusCreated)
}

// settled words an Edit outcome: nil when it ended in an accepted status.
func settled(doc *docwrite.Doc, result *docwrite.Result, err error, accepted ...string) error {
	if err != nil {
		return fmt.Errorf("hub %s not updated: %w", doc.Path, err)
	}
	if slices.Contains(accepted, result.Response.Status) {
		return nil
	}
	return fmt.Errorf("hub %s not updated: %s", doc.Path, result.Response.Status)
}

// Reaches reports a link from the hub to target: the target itself or, for
// a directory, anything under it.
func Reaches(body, hubPath, target string) bool {
	for _, dest := range links.Extract(body) {
		if resolved := links.ResolvePath(hubPath, dest); resolved != "" && matches(resolved, target) {
			return true
		}
	}
	return false
}

// matches is one resolved link against one target: the document, or the
// directory (slash optional) and anything under it.
func matches(resolved, target string) bool {
	return resolved == target || resolved == strings.TrimSuffix(target, "/") ||
		(strings.HasSuffix(target, "/") && strings.HasPrefix(resolved, target))
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

// Drop names what a hub must stop linking: document paths, or directories
// with their slash (anything under them), and the identity to write under.
type Drop struct {
	Targets []string
	Stamp   map[string]string
}

// DropLinks removes every bullet whose links all point at a drop target; a
// bullet with a link elsewhere stays. One compare and swap, one retry; head
// nil fetches. An absent or archived hub links nothing and is not an error.
func DropLinks(ctx context.Context, doc *docwrite.Doc, head *docwrite.Head, drop *Drop) (dropped int, err error) {
	result, _, err := doc.Edit(ctx, head, func(h *docwrite.Head) (docwrite.Write, bool, error) {
		if h.Status != protocol.StatusOK {
			return docwrite.Write{}, false, nil
		}
		body, n := WithoutLinks(h.Body, doc.Path, drop.Targets)
		dropped = n
		if n == 0 {
			return docwrite.Write{}, false, nil
		}
		return docwrite.Write{Body: body, Metadata: stamped(docwrite.Carried(h.Metadata), drop.Stamp)}, true, nil
	})
	err = settled(doc, &result, err, protocol.StatusOK, protocol.StatusNotFound, protocol.StatusArchived)
	if err != nil {
		return 0, err
	}
	return dropped, nil
}

// Ancestors are the index.md of every directory above the path, nearest
// first, the path's own hub excluded.
func Ancestors(p string) []string {
	var hubs []string
	dir := path.Dir(strings.TrimSuffix(p, "/"))
	for {
		if h := path.Join(dir, "index.md"); h != p {
			hubs = append(hubs, h)
		}
		if dir == "/" || dir == "." {
			return hubs
		}
		dir = path.Dir(dir)
	}
}

// Dropped is one ancestor hub's outcome: bullets dropped, or why not.
type Dropped struct {
	Hub     string
	Dropped int
	Err     error
}

// DropFromAncestors drops the targets' bullets from every ancestor hub of p;
// one hub's failure does not stop the next.
func DropFromAncestors(ctx context.Context, docs Docs, p string, drop *Drop) []Dropped {
	hubs := Ancestors(p)
	out := make([]Dropped, 0, len(hubs))
	for _, hubPath := range hubs {
		n, err := DropLinks(ctx, docs(hubPath), nil, drop)
		out = append(out, Dropped{Hub: hubPath, Dropped: n, Err: err})
	}
	return out
}

// WithoutLinks is the body with every bullet dropped whose links (at least
// one) all reach a target; n counts them. Links are read from one parse of
// the body and judged by the line they start on: a hub bullet is one line.
func WithoutLinks(body, hubPath string, targets []string) (out string, n int) {
	byLine := map[int][]string{}
	for _, link := range links.ExtractWithPositions(body) {
		pos := link.OpenBracket
		if pos < 0 {
			pos = link.BlockStart
		}
		if pos >= 0 {
			byLine[strings.Count(body[:pos], "\n")] = append(byLine[strings.Count(body[:pos], "\n")], link.Dest)
		}
	}
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for i, line := range lines {
		if bulletLine.MatchString(line) && onlyReaches(byLine[i], hubPath, targets) {
			n++
			continue
		}
		kept = append(kept, line)
	}
	if n == 0 {
		return body, 0
	}
	return strings.Join(kept, "\n"), n
}

var bulletLine = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)

// onlyReaches: every link resolves to a target, and there is one.
func onlyReaches(dests []string, hubPath string, targets []string) bool {
	if len(dests) == 0 {
		return false
	}
	for _, dest := range dests {
		resolved := links.ResolvePath(hubPath, dest)
		if resolved == "" || !slices.ContainsFunc(targets, func(t string) bool { return matches(resolved, t) }) {
			return false
		}
	}
	return true
}
