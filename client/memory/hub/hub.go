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
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/mdoutline"
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
	Section    string // "## Journal"; "" takes the siblings' section, else the end
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
		r := links.ResolverFor(doc.Host)
		switch PlaceOf(r, h.Body, doc.Path, link.Target) {
		case Named:
			return docwrite.Write{}, false, nil
		case AtCap:
			return docwrite.Write{}, false, ErrAtCap
		}
		section := link.Section
		if section == "" {
			section = SectionFor(r, h.Body, doc.Path, link.Target)
		}
		return docwrite.Write{Body: WithBullet(h.Body, section, link.Bullet), Metadata: stamped(docwrite.Carried(h.Metadata), link.Stamp)}, true, nil
	})
	return added, settled(doc, &result, err)
}

// Place is where a target stands in a hub.
type Place int

// Open has room for a bullet; Named, the hub reaches the target already;
// AtCap, the hub carries gate.HubLinkMax documents.
const (
	Open Place = iota
	Named
	AtCap
)

// ErrAtCap is EnsureLink's refusal when the hub has no room.
var ErrAtCap = fmt.Errorf("at the cap of %d links; add a second-level hub, or link the directory so the hub names every document in it", gate.HubLinkMax)

// PlaceOf is the one rule for adding a link: a target the hub reaches needs
// nothing, a hub at the cap takes nothing, else a bullet goes in.
func PlaceOf(r links.Resolver, body, hubPath, target string) Place {
	switch {
	case reaches(r, body, hubPath, target):
		return Named
	case gate.OutboundDocs(body) >= gate.HubLinkMax:
		return AtCap
	}
	return Open
}

// reaches is the link a target needs: a directory, anything under it; a
// document, the document itself or its directory.
func reaches(r links.Resolver, body, hubPath, target string) bool {
	if strings.HasSuffix(target, "/") {
		return Reaches(r, body, hubPath, target)
	}
	return Names(r, body, hubPath, target)
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
// a directory, anything under it. r resolves links on the hub's store.
func Reaches(r links.Resolver, body, hubPath, target string) bool {
	for _, dest := range links.Extract(body) {
		if resolved := r.Path(hubPath, dest); resolved != "" && matches(resolved, target) {
			return true
		}
	}
	return false
}

// Names reports a link to the document, or to its directory, which names
// every document directly in it (a journal hub links journal/, a collection
// hub the directory its views list); a sibling's link is neither.
func Names(r links.Resolver, body, hubPath, docPath string) bool {
	dir := path.Dir(docPath)
	for _, dest := range links.Extract(body) {
		switch r.Path(hubPath, dest) {
		case docPath, dir, dir + "/":
			return true
		}
	}
	return false
}

// SectionFor is the "## " heading of the section whose links name the most
// documents beside the target, so a new bullet joins its siblings; "" when
// no section names one, which appends at the end.
func SectionFor(r links.Resolver, body, hubPath, target string) string {
	dir := path.Dir(strings.TrimSuffix(target, "/"))
	sections := slices.DeleteFunc(mdoutline.Headings(body), func(h mdoutline.Heading) bool { return h.Level != 2 })
	counts := make([]int, len(sections))
	best := -1
	for _, info := range links.ExtractWithPositions(body) {
		resolved := r.Path(hubPath, info.Dest)
		if resolved == "" || path.Dir(strings.TrimSuffix(resolved, "/")) != dir {
			continue
		}
		pos := info.Pos()
		if pos < 0 {
			continue
		}
		// Sections are in Start order; the last one at or before the link holds it.
		i := sort.Search(len(sections), func(j int) bool { return sections[j].Start > pos }) - 1
		if i < 0 {
			continue
		}
		counts[i]++
		if best < 0 || counts[i] > counts[best] {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return "## " + sections[best].Text
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
		body, n := WithoutLinks(links.ResolverFor(doc.Host), h.Body, doc.Path, drop.Targets)
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

// Paths are where a directory's hub may live, in the order the rule prefers:
// its index.md, else the document named after it (/a/b.md for /a/b/), where
// curate's split puts a document's topics. The root has only /index.md.
func Paths(dir string) []string {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" || dir == "." {
		return []string{"/index.md"}
	}
	return []string{dir + "/index.md", dir + ".md"}
}

// Ancestors are the hubs that may name the path, nearest directory first,
// each directory's Paths in order. An index.md skips its own directory; a
// directory skips its index.md (archived with it), not its <dir>.md.
func Ancestors(p string) []string {
	if strings.HasSuffix(p, "/") {
		return append(Paths(p)[1:], Ancestors(p+"index.md")...)
	}
	var hubs []string
	for dir := path.Dir(p); ; dir = path.Dir(dir) {
		if !slices.Contains(Paths(dir), p) {
			hubs = append(hubs, Paths(dir)...)
		}
		if dir == "/" || dir == "." {
			return hubs
		}
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
func WithoutLinks(r links.Resolver, body, hubPath string, targets []string) (out string, n int) {
	out, cut := cutLinks(r, body, hubPath, &Move{Targets: targets})
	return out, len(cut)
}

// Move names the bullets MoveLinks cuts (those reaching only Targets) and
// the one bullet that takes the first one's line; "" leaves none.
type Move struct {
	Targets []string
	Bullet  string
}

// MoveLinks cuts the bullets WithoutLinks drops and puts m.Bullet where the
// first was. Moved are the cut lines with relative links made absolute, so
// another hub can carry them verbatim.
func MoveLinks(r links.Resolver, body, hubPath string, m *Move) (out string, moved []string) {
	out, moved = cutLinks(r, body, hubPath, m)
	for i, line := range moved {
		for _, link := range links.ExtractWithPositions(line) {
			if abs := r.Path(hubPath, link.Dest); abs != "" && abs != link.Dest {
				line = strings.Replace(line, "]("+link.Dest, "]("+abs, 1)
			}
		}
		moved[i] = line
	}
	return out, moved
}

// cutLinks drops the bullets whose links all reach a target, m.Bullet (when
// not "") taking the first one's line, and returns the dropped lines.
func cutLinks(r links.Resolver, body, hubPath string, m *Move) (out string, cut []string) {
	byLine := map[int][]string{}
	for _, link := range links.ExtractWithPositions(body) {
		if pos := link.Pos(); pos >= 0 {
			byLine[strings.Count(body[:pos], "\n")] = append(byLine[strings.Count(body[:pos], "\n")], link.Dest)
		}
	}
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for i, line := range lines {
		if !bulletLine.MatchString(line) || !onlyReaches(r, byLine[i], hubPath, m.Targets) {
			kept = append(kept, line)
			continue
		}
		if len(cut) == 0 && m.Bullet != "" {
			kept = append(kept, m.Bullet)
		}
		cut = append(cut, line)
	}
	if len(cut) == 0 {
		return body, nil
	}
	return strings.Join(kept, "\n"), cut
}

var bulletLine = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)

// onlyReaches: every link resolves to a target, and there is one.
func onlyReaches(r links.Resolver, dests []string, hubPath string, targets []string) bool {
	if len(dests) == 0 {
		return false
	}
	for _, dest := range dests {
		resolved := r.Path(hubPath, dest)
		if resolved == "" || !slices.ContainsFunc(targets, func(t string) bool { return matches(resolved, t) }) {
			return false
		}
	}
	return true
}
