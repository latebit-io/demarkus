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
// else the bullet added (or the hub created) at the head's version, one
// retry on conflict; added reports a write that landed. Head nil fetches.
func EnsureLink(ctx context.Context, doc *docwrite.Doc, head *docwrite.Head, link *Link) (added bool, err error) {
	result, added, err := doc.Edit(ctx, head, func(h *docwrite.Head) (docwrite.Write, bool, error) {
		if h.Status == protocol.StatusNotFound {
			return docwrite.Write{Body: link.CreateBody, Metadata: stamped(link.CreateMeta, link.Stamp)}, true, nil
		}
		if reaches(h.Body, doc.Path, link.Target) {
			return docwrite.Write{}, false, nil
		}
		return docwrite.Write{Body: WithBullet(h.Body, link.Section, link.Bullet), Metadata: stamped(docwrite.Carried(h.Metadata), link.Stamp)}, true, nil
	})
	return added, settled(doc, &result, err)
}

// reaches is the link a target needs: a directory, anything under it; a
// document, the document itself or its directory.
func reaches(body, hubPath, target string) bool {
	if strings.HasSuffix(target, "/") {
		return Reaches(body, hubPath, target)
	}
	return Names(body, hubPath, target)
}

// settled words an Edit outcome: nil when the write landed or the head
// ended in a status listed as unchanged.
func settled(doc *docwrite.Doc, result *docwrite.Result, err error, unchanged ...string) error {
	if err != nil {
		return fmt.Errorf("hub %s not updated: %w", doc.Path, err)
	}
	if s := result.Response.Status; protocol.IsWriteSuccess(s) || slices.Contains(unchanged, s) {
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

// Names reports a link to the document, or to its directory, which names
// every document directly in it (a journal hub links journal/, not each
// day); a sibling's link is neither.
func Names(body, hubPath, docPath string) bool {
	dir := path.Dir(docPath)
	for _, dest := range links.Extract(body) {
		switch links.ResolvePath(hubPath, dest) {
		case docPath, dir, dir + "/":
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
	if section == "" {
		return joinBullet(body, bullet)
	}
	body = strings.TrimRight(body, "\n") + "\n"
	headings := mdoutline.Headings(body)
	for i, h := range headings {
		if "## "+h.Text != section {
			continue
		}
		end := len(body)
		if i+1 < len(headings) {
			end = headings[i+1].Start
		}
		if end == len(body) {
			return joinBullet(body, bullet)
		}
		return joinBullet(body[:end], bullet) + "\n" + body[end:]
	}
	return body + "\n" + section + "\n\n" + bullet + "\n"
}

// joinBullet appends the bullet to the block: a blank line opens a list
// after prose; a block ending in a list item continues it.
func joinBullet(block, bullet string) string {
	block = strings.TrimRight(block, "\n")
	sep := "\n"
	if !bulletLine.MatchString(block[strings.LastIndex(block, "\n")+1:]) {
		sep = "\n\n"
	}
	return block + sep + bullet + "\n"
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
	err = settled(doc, &result, err, protocol.StatusNotFound, protocol.StatusArchived)
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

// AncestorsOf is the ordered union of every path's ancestor hubs, nearest
// first, each hub once.
func AncestorsOf(paths ...string) []string {
	var hubs []string
	for _, p := range paths {
		for _, h := range Ancestors(p) {
			if !slices.Contains(hubs, h) {
				hubs = append(hubs, h)
			}
		}
	}
	slices.SortStableFunc(hubs, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	return hubs
}

// DropFromHubs drops the targets' bullets from each hub; one hub's failure
// does not stop the next.
func DropFromHubs(ctx context.Context, docs Docs, hubs []string, drop *Drop) []Dropped {
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
