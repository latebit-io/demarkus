// Package curate brings one document into shape: the style findings on it,
// and the mechanical fix it can propose (the edits the gate's rules imply,
// or a split into a hub plus topic files once it outgrows one fetch). A
// surface shows the proposal and applies it only against its hash.
package curate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/docwrite/gate"
	"github.com/latebit-io/demarkus/client/hub"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
)

// frameCap: a section under it is the document's own frame and stays in
// the hub while the hub fits in one fetch.
const frameCap = 1024

// Input is the document as fetched.
type Input struct {
	Path     string
	Body     string
	Version  int
	Metadata map[string]string
}

// Write is one document the proposal publishes.
type Write struct {
	Path     string
	Body     string
	Metadata map[string]string
}

// Kinds of proposal.
const (
	KindClean  = "clean"  // no findings
	KindReport = "report" // findings, nothing mechanical to do
	KindEdit   = "edit"   // the document rewritten in place
	KindSplit  = "split"  // a hub plus topic files
)

// Proposal is the report and, when there is one, the change to apply.
type Proposal struct {
	Path     string
	Version  int
	Size     int
	Problems []string // findings on the document as it is
	Kind     string
	Writes   []Write  // topics first, the document last
	Fixed    []string // what the change settles
	Unfixed  []string // findings left to the agent, or why nothing is proposed
	// Moved maps an anchor of the document to where the change puts it
	// (path#anchor), so inbound links can be repointed.
	Moved map[string]string
	Hash  string // binds an apply to this proposal at this version
}

// Propose reports the document and proposes the smallest mechanical change
// that settles what it can. Nothing is written.
func Propose(in *Input) Proposal {
	p := Proposal{Path: in.Path, Version: in.Version, Size: len(in.Body), Problems: gate.AuditProblems(in.Path, in.Body, in.Metadata["description"])}
	if len(p.Problems) == 0 {
		p.Kind = KindClean
		return p
	}
	headings := mdoutline.Headings(in.Body)
	if len(in.Body) >= mdoutline.OutlineThreshold {
		split(in, headings, &p)
	} else {
		edit(in, headings, &p)
	}
	if len(p.Writes) == 0 {
		p.Kind = KindReport
		return p
	}
	p.Hash = hash(in.Version, p.Writes)
	return p
}

// edit rewrites the document in place: the frontmatter fence into metadata,
// em dashes to commas, status out of headings, the description as the
// missing summary. What needs prose stays unfixed.
func edit(in *Input, headings []mdoutline.Heading, p *Proposal) {
	body, meta := in.Body, docwrite.Carried(in.Metadata)
	if kv, rest, ok := gate.Frontmatter(body); ok {
		body = rest
		p.Fixed = append(p.Fixed, "frontmatter fence removed; metadata keys: "+strings.Join(Lift(kv, meta), ", "))
		headings = mdoutline.Headings(body)
	}
	// Headings are renamed on the offsets just parsed; the em dash pass
	// comes after since it moves offsets but no heading.
	var renamed []rename
	body, renamed = stripHeadingStatus(body, headings)
	for _, r := range renamed {
		p.Fixed = append(p.Fixed, fmt.Sprintf("heading %q is now %q with its status on the line under it", r.from, r.to))
	}
	if total := gate.EmDashCount(body); total > 0 {
		var n int
		body, n = replaceEmDashes(body)
		if n > 0 {
			p.Fixed = append(p.Fixed, fmt.Sprintf("%d of %d em dash(es) replaced with a comma; the rest are in code or open a line", n, total))
		}
	}
	after := mdoutline.Headings(body)
	if desc := strings.TrimSpace(meta["description"]); desc != "" && gate.MissingSummary(body, after) {
		body = withSummary(body, after, desc)
		after = mdoutline.Headings(body)
		p.Fixed = append(p.Fixed, "metadata.description added as the summary under the H1")
	}
	if body == in.Body {
		return
	}
	p.Kind = KindEdit
	p.Writes = []Write{{Path: in.Path, Body: body, Metadata: meta}}
	p.Unfixed = gate.AuditProblems(in.Path, body, meta["description"])
	// Headings keep their count and order through a rename, so index maps
	// the old anchor to the new one.
	for _, r := range renamed {
		if before, now := headings[r.index].Anchor, after[r.index].Anchor; before != now {
			p.moved(before, in.Path+"#"+now)
		}
	}
}

func (p *Proposal) moved(anchor, to string) {
	if p.Moved == nil {
		p.Moved = map[string]string{}
	}
	p.Moved[anchor] = to
}

var (
	parenGroup    = regexp.MustCompile(`\s*\([^()]*\)`)
	lineStartDash = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])?\s*—`)
)

// replaceEmDashes turns em dashes into commas outside code and line starts,
// where a dash may be a bullet; n is what changed.
func replaceEmDashes(body string) (out string, n int) {
	code := codeMask{segs: mdoutline.CodeSegments(body)}
	var b strings.Builder
	b.Grow(len(body))
	for off := 0; off <= len(body); {
		end := len(body)
		if i := strings.IndexByte(body[off:], '\n'); i >= 0 {
			end = off + i
		}
		keep := off
		if m := lineStartDash.FindStringIndex(body[off:end]); m != nil {
			keep = off + m[1]
		}
		b.WriteString(body[off:keep])
		line, changed := replaceOutsideCode(body, keep, end, &code)
		b.Write(line)
		n += changed
		if end < len(body) {
			b.WriteByte('\n')
		}
		off = end + 1
	}
	return b.String(), n
}

const emDash = "—"

// codeMask answers whether a byte offset is code; offsets are asked in order.
type codeMask struct {
	segs []mdoutline.Segment
	i    int
}

func (m *codeMask) at(off int) bool {
	for m.i < len(m.segs) && m.segs[m.i].Stop <= off {
		m.i++
	}
	return m.i < len(m.segs) && off >= m.segs[m.i].Start
}

// replaceOutsideCode rewrites body[from:to] with each em dash outside code
// replaced by ", ", surrounding blanks folded; n is what changed.
func replaceOutsideCode(body string, from, to int, code *codeMask) (out []byte, n int) {
	out = make([]byte, 0, to-from)
	for i := from; i < to; {
		if !strings.HasPrefix(body[i:to], emDash) || code.at(i) {
			out = append(out, body[i])
			i++
			continue
		}
		n++
		out = append(bytes.TrimRight(out, " \t"), ", "...)
		i += len(emDash)
		for i < to && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
	}
	return out, n
}

// Lift moves the fence's keys into meta (a key the document already carries
// wins; name is the title; reserved and identity keys never) and names them.
func Lift(kv, meta map[string]string) []string {
	var keys []string
	for _, k := range slices.Sorted(maps.Keys(kv)) {
		v := strings.TrimSpace(kv[k])
		k = strings.ToLower(k)
		if k == "name" {
			k = "title"
		}
		if _, taken := meta[k]; v == "" || taken || protocol.IsReservedMetadataKey(k) || protocol.IsIdentityMetadataKey(k) {
			continue
		}
		meta[k] = v
		keys = append(keys, k)
	}
	return keys
}

type rename struct {
	index    int // in the headings of the body as given
	from, to string
}

// stripHeadingStatus moves the status tokens of every heading below the H1
// onto a "Status:" line under it; a heading left empty stays as it is.
// Edits run back to front so earlier offsets stay valid.
func stripHeadingStatus(body string, headings []mdoutline.Heading) (string, []rename) {
	var renamed []rename
	for i := len(headings) - 1; i >= 0; i-- {
		h := headings[i]
		if h.Level == 1 || !gate.CarriesStatus(h.Text) {
			continue
		}
		to, status := withoutStatus(h.Text)
		if to == "" || to == h.Text {
			continue
		}
		line, rest, _ := strings.Cut(body[h.Start:h.End], "\n")
		if !strings.Contains(line, h.Text) {
			continue // setext or inline markup the rename cannot place
		}
		section := strings.Replace(line, h.Text, to, 1) + "\n\nStatus: " + status + "\n\n" + strings.TrimLeft(rest, "\n")
		body = body[:h.Start] + section + body[h.End:]
		renamed = append(renamed, rename{index: i, from: h.Text, to: to})
	}
	slices.Reverse(renamed)
	return body, renamed
}

// withoutStatus removes a parenthesized group holding a status token whole,
// then any bare token; the removed text is the status line.
func withoutStatus(text string) (name, status string) {
	var removed []string
	text = parenGroup.ReplaceAllStringFunc(text, func(group string) string {
		inner := strings.TrimSpace(strings.Trim(strings.TrimSpace(group), "()"))
		if !gate.CarriesStatus(inner) {
			return group
		}
		removed = append(removed, inner)
		return ""
	})
	for _, re := range gate.StatusTokens {
		text = re.ReplaceAllStringFunc(text, func(tok string) string {
			removed = append(removed, tok)
			return ""
		})
	}
	return strings.Trim(gate.Collapse(text), " :,-"), strings.Join(removed, " ")
}

// withSummary puts the sentence under the first H1.
func withSummary(body string, headings []mdoutline.Heading, summary string) string {
	for _, h := range headings {
		if h.Level != 1 {
			continue
		}
		line, rest, _ := strings.Cut(body[h.Start:], "\n")
		return body[:h.Start] + line + "\n\n" + summary + "\n\n" + strings.TrimLeft(rest, "\n")
	}
	return body
}

// section is one H2 span of the document and where it may go.
type section struct {
	heading   mdoutline.Heading
	text      string // the heading line through the end of its span
	candidate string // its topic path
	bullet    string // its line in the hub's topics list
	topic     string // candidate once it moves; "" stays in the hub
}

// split moves every second-level section of weight into a topic file under
// <path without .md>/, headings verbatim so anchors survive, and leaves the
// document as the hub: its preamble, a links list, and its short sections.
func split(in *Input, headings []mdoutline.Heading, p *Proposal) {
	sections, refusal := planSections(in, headings)
	if refusal != "" {
		p.Unfixed = append(p.Unfixed, refusal)
		return
	}
	preamble := strings.TrimRight(in.Body[:sections[0].heading.Start], "\n")
	// Short sections stay only while the hub fits in one fetch.
	for len(hubBody(preamble, sections, headings)) >= mdoutline.OutlineThreshold {
		largest := -1
		for i := range sections {
			if sections[i].topic == "" && (largest < 0 || len(sections[i].text) > len(sections[largest].text)) {
				largest = i
			}
		}
		if largest < 0 {
			break
		}
		sections[largest].topic = sections[largest].candidate
	}
	title := hub.Title(in.Body, in.Metadata, strings.TrimSuffix(path.Base(in.Path), ".md"))
	carried := docwrite.Carried(in.Metadata)
	for i := range sections {
		s := &sections[i]
		if s.topic == "" {
			continue
		}
		w := topicWrite(title, in.Path, carried, s)
		p.Writes = append(p.Writes, w)
		p.Fixed = append(p.Fixed, fmt.Sprintf("%q moved to %s (%d bytes)", s.heading.Text, s.topic, len(w.Body)))
		for _, h := range headings {
			if h.Start >= s.heading.Start && h.Start < s.heading.End {
				p.moved(h.Anchor, s.topic+"#"+h.Anchor)
			}
		}
	}
	body := hubBody(preamble, sections, headings)
	meta := maps.Clone(carried)
	meta["tags"] = strings.Join(hubTags(preamble, sections, carried["tags"]), ",")
	p.Writes = append(p.Writes, Write{Path: in.Path, Body: body, Metadata: meta})
	p.Fixed = append(p.Fixed, fmt.Sprintf("%s becomes the hub (%d bytes)", in.Path, len(body)))
	p.Kind = KindSplit
	p.Unfixed = gate.AuditProblems(in.Path, body, meta["description"])
}

// planSections cuts the body at its H2 headings, names each section's topic
// file and bullet, and moves the sections of weight; a document it cannot
// cut or name is a refusal.
func planSections(in *Input, headings []mdoutline.Heading) (sections []section, refusal string) {
	for _, h := range headings {
		if h.Level == 2 {
			sections = append(sections, section{heading: h, text: in.Body[h.Start:h.End]})
		}
	}
	if len(sections) == 0 {
		return nil, "no second-level headings to split on; add `## ` sections first"
	}
	dir := strings.TrimSuffix(in.Path, ".md") + "/"
	seen := map[string]bool{}
	for i := range sections {
		s := &sections[i]
		slug := mdoutline.Slug(s.heading.Text)
		if slug == "" || slug == "index" {
			slug += "-topic"
		}
		if seen[slug] {
			return nil, fmt.Sprintf("two sections slug to %q; rename one before splitting", slug)
		}
		seen[slug] = true
		s.candidate = dir + slug + ".md"
		s.bullet = fmt.Sprintf("- [%s](%s)", s.heading.Text, s.candidate)
		if line := oneLine(s.text, utf8.RuneCountInString(s.heading.Text)); line != "" {
			s.bullet += ": " + line
		}
		if len(s.text) >= frameCap {
			s.topic = s.candidate
		}
	}
	return sections, ""
}

// topicWrite is one moved section as its own document: a derived H1, a link
// back to the hub, the section verbatim; tags are the parent's name, its
// slug and the parent tags it mentions.
func topicWrite(title, parentPath string, carried map[string]string, s *section) Write {
	tags := protocol.MergeTags([]string{strings.TrimSuffix(path.Base(parentPath), ".md"), mdoutline.Slug(s.heading.Text)}, nil)
	lower := strings.ToLower(s.text)
	for _, tag := range protocol.SplitTags(carried["tags"]) {
		if mentions(lower, tag) {
			tags = protocol.MergeTags(tags, []string{tag})
		}
	}
	meta := map[string]string{"tags": strings.Join(tags, ","), "importance": topicImportance(carried["importance"]), CuratedFrom: parentPath}
	if t := carried["type"]; t != "" {
		meta["type"] = t
	}
	body := fmt.Sprintf("# %s: %s\n\nPart of [%s](%s).\n\n%s\n", title, s.heading.Text, title, parentPath, strings.TrimRight(s.text, "\n"))
	meta[curatedDigest] = topicDigest(body, meta)
	return Write{Path: s.topic, Body: body, Metadata: meta}
}

// hubTags drops a tag the hub's own prose never mentions when a topic does;
// the links list naming a topic is not a mention.
func hubTags(preamble string, sections []section, tags string) []string {
	var own, moved strings.Builder
	own.WriteString(preamble)
	for i := range sections {
		if sections[i].topic == "" {
			own.WriteString(sections[i].text)
		} else {
			moved.WriteString(sections[i].text)
		}
	}
	ownLower, movedLower := strings.ToLower(own.String()), strings.ToLower(moved.String())
	var kept []string
	for _, tag := range protocol.SplitTags(tags) {
		if mentions(ownLower, tag) || !mentions(movedLower, tag) {
			kept = append(kept, tag)
		}
	}
	return kept
}

// hubBody is the document after the split: preamble, the topics list, then
// the sections that stay, in their order.
func hubBody(preamble string, sections []section, headings []mdoutline.Heading) string {
	var b strings.Builder
	b.WriteString(preamble)
	list := "Topics"
	for _, h := range headings {
		if h.Text == list {
			list = "Topic files"
		}
	}
	fmt.Fprintf(&b, "\n\n## %s\n\n", list)
	for i := range sections {
		if sections[i].topic != "" {
			b.WriteString(sections[i].bullet + "\n")
		}
	}
	for i := range sections {
		if sections[i].topic == "" {
			b.WriteString("\n" + strings.TrimRight(sections[i].text, "\n") + "\n")
		}
	}
	return b.String()
}

// oneLine is a section's first sentence, cut so the bullet (label plus
// line) stays under the gate's visible cap.
func oneLine(sectionText string, labelLen int) string {
	_, rest, _ := strings.Cut(sectionText, "\n")
	para := gate.Collapse(mdoutline.OpeningParagraph(rest))
	if i := strings.Index(para, ". "); i > 0 {
		para = para[:i+1]
	}
	cut := gate.ClipBullet(para, gate.HubBulletMax-labelLen-6) // ": " and "..." with room to spare
	if cut != para {
		cut += "..."
	}
	return cut
}

// mentions is a case-insensitive match of a tag in lowercased text; a
// hyphenated tag also matches with spaces.
func mentions(lower, tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	return tag != "" && (strings.Contains(lower, tag) || strings.Contains(lower, strings.ReplaceAll(tag, "-", " ")))
}

// topicImportance keeps the parent's unless it is hub-grade.
func topicImportance(parent string) string {
	if f, err := strconv.ParseFloat(parent, 64); err == nil && f < 0.8 {
		return parent
	}
	return "0.7"
}

// hash binds an apply to exactly these writes from exactly this version.
func hash(version int, writes []Write) string {
	var b strings.Builder
	fmt.Fprintf(&b, "v%d\n", version)
	for i := range writes {
		w := &writes[i]
		fmt.Fprintf(&b, "%s\n%d\n%s\n", w.Path, len(w.Body), w.Body)
		for _, k := range slices.Sorted(maps.Keys(w.Metadata)) {
			fmt.Fprintf(&b, "%s=%s\n", k, w.Metadata[k])
		}
	}
	return docwrite.Digest(b.String())
}

// Inbound is one link into the document that names a section.
type Inbound struct {
	Source string // the linking document, as the surface names it
	Anchor string // the fragment, no '#'
}

// Source is a linking document as read: its name for the report, its store
// path for relative links, its body.
type Source struct{ Name, Path, Body string }

// InboundFrom reads the anchored links in the source that point at the
// document: by store path, or by the document's canonical URL.
func InboundFrom(src *Source, targetPath, targetURL string) []Inbound {
	var out []Inbound
	want := links.CanonicalURL(targetURL)
	for _, link := range mdoutline.AnchoredLinks(src.Body) {
		if link.Fragment == "" {
			continue
		}
		if links.ResolvePath(src.Path, link.Dest) == targetPath || (want != "" && links.CanonicalURL(links.Resolve(targetURL, link.Dest)) == want) {
			out = append(out, Inbound{Source: src.Name, Anchor: link.Fragment})
		}
	}
	return out
}

// Move is an inbound link to repoint: From and To are path#anchor.
type Move struct{ Source, From, To string }

// Moves are the inbound links the proposal's change breaks, with their new
// targets.
func (p *Proposal) Moves(inbound []Inbound) []Move {
	var moves []Move
	for _, link := range inbound {
		if to, ok := p.Moved[link.Anchor]; ok {
			moves = append(moves, Move{Source: link.Source, From: p.Path + "#" + link.Anchor, To: to})
		}
	}
	return moves
}

// Report renders the proposal for the agent to show: the findings, the
// change, what is left, the links to repoint, then every body in full.
func (p *Proposal) Report(moves []Move, notes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\npath: %s\nversion: %d\nsize: %d bytes\n\n## Checks\n\n", p.Kind, p.Path, p.Version, p.Size)
	if len(p.Problems) == 0 {
		b.WriteString("- all checks pass\n")
	}
	for _, problem := range p.Problems {
		b.WriteString("- " + problem + "\n")
	}
	if p.Kind == KindReport {
		b.WriteString("\nNothing mechanical to propose; the findings above need the agent.\n")
		for _, u := range p.Unfixed {
			b.WriteString("- " + u + "\n")
		}
	}
	if len(p.Writes) > 0 {
		fmt.Fprintf(&b, "\n## Proposal: %s\n\n", p.Kind)
		for _, f := range p.Fixed {
			b.WriteString("- " + f + "\n")
		}
		if len(p.Unfixed) > 0 {
			b.WriteString("\nLeft to the agent after this change:\n\n")
			for _, u := range p.Unfixed {
				b.WriteString("- " + u + "\n")
			}
		}
	}
	if len(moves) > 0 {
		b.WriteString("\nInbound links to repoint:\n\n")
		for _, m := range moves {
			fmt.Fprintf(&b, "- %s: %s -> %s\n", m.Source, m.From, m.To)
		}
	}
	for _, note := range notes {
		b.WriteString("\nnote: " + note + "\n")
	}
	if len(p.Writes) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "\nproposal: %s\nApply with apply: true and this proposal; it binds to v%d.\n", p.Hash, p.Version)
	for i := range p.Writes {
		w := &p.Writes[i]
		fmt.Fprintf(&b, "\n=== %s (%s)\n%s", w.Path, metaLine(w.Metadata), w.Body)
	}
	return b.String()
}

func metaLine(meta map[string]string) string {
	parts := make([]string, 0, len(meta))
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		parts = append(parts, k+": "+meta[k])
	}
	return strings.Join(parts, "; ")
}

// curatedDigest is the metadata key a topic carries over the body and
// metadata curate wrote, so a resumed split can tell its own write from an
// edit made since.
const curatedDigest = "curated-digest"

// topicDigest is the digest over a topic's body and metadata, the digest
// key itself excluded.
func topicDigest(body string, meta map[string]string) string {
	meta = sansDigest(meta)
	parts := make([]string, 0, 1+2*len(meta))
	parts = append(parts, body)
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		parts = append(parts, k, meta[k])
	}
	return docwrite.Digest(parts...)
}

// sansDigest is the metadata without the digest key; identical content is
// identical whether or not a topic carries one.
func sansDigest(meta map[string]string) map[string]string {
	out := maps.Clone(meta)
	delete(out, curatedDigest)
	return out
}

// CuratedFrom is the metadata key a topic carries naming the document it
// was split from, so a resumed split may update it and nothing else may.
const CuratedFrom = "curated-from"

// Applied is one write the apply made or found in place.
type Applied struct {
	Path     string
	Action   string // created, skipped, updated
	Version  int
	Body     string
	Metadata map[string]string // as sent, plus the response's keys
}

// Actions an apply reports.
const (
	ActionCreated = "created"
	ActionSkipped = "skipped"
	ActionUpdated = "updated"
)

// Refusal is an apply the store's state stopped: what was in the way.
type Refusal struct{ Path, Reason string }

func (r *Refusal) Error() string { return r.Path + ": " + r.Reason }

// Apply preflights every topic path (absent created, identical skipped, an
// earlier apply's topic updated, anything else refused before a write), then
// writes the topics and the document at its version. What landed is returned.
func Apply(ctx context.Context, docs hub.Docs, p *Proposal, stamp map[string]string) (applied []Applied, err error) {
	if len(p.Writes) == 0 {
		return nil, fmt.Errorf("nothing to apply: the document is %s", p.Kind)
	}
	a := applier{docs: docs, stamp: stamp}
	topics := p.Writes[:len(p.Writes)-1]
	// Per topic: -1 skips an identical one, 0 creates, else updates at that
	// version a topic an earlier apply of this document wrote and nobody
	// edited since (its digest still matches).
	expected := make([]int, len(topics))
	for i := range topics {
		w := &topics[i]
		h, err := docs(w.Path).Head(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", w.Path, err)
		}
		switch {
		case h.Status == protocol.StatusNotFound:
		case h.Status != protocol.StatusOK:
			return nil, &Refusal{Path: w.Path, Reason: "fetch: " + h.Status + "; nothing written"}
		case h.Body == w.Body && maps.Equal(sansDigest(docwrite.Carried(h.Metadata)), sansDigest(w.Metadata)):
			expected[i] = -1
		case h.Metadata[CuratedFrom] == p.Path && h.Metadata[curatedDigest] == topicDigest(h.Body, docwrite.Carried(h.Metadata)) && h.Version > 0:
			expected[i] = h.Version
		default:
			return nil, &Refusal{Path: w.Path, Reason: "exists and differs from the proposal; nothing written"}
		}
	}
	for i := range topics {
		w := &topics[i]
		action := ActionCreated
		switch {
		case expected[i] < 0:
			applied = append(applied, Applied{Path: w.Path, Action: ActionSkipped, Body: w.Body, Metadata: w.Metadata})
			continue
		case expected[i] > 0:
			action = ActionUpdated
		}
		done, err := a.publish(ctx, w, max(expected[i], 0), action)
		if err != nil {
			return applied, err
		}
		applied = append(applied, done)
	}
	doc := &p.Writes[len(p.Writes)-1]
	done, err := a.publish(ctx, doc, p.Version, ActionUpdated)
	if err != nil {
		return applied, err
	}
	return append(applied, done), nil
}

// applier writes one proposal under one identity.
type applier struct {
	docs  hub.Docs
	stamp map[string]string
}

// publish is one version checked write under the surface's identity.
func (a *applier) publish(ctx context.Context, w *Write, expected int, action string) (Applied, error) {
	meta := maps.Clone(w.Metadata)
	maps.Copy(meta, a.stamp)
	result, err := a.docs(w.Path).Publish(ctx, docwrite.Write{Body: w.Body, ExpectedVersion: expected, Metadata: meta}, merge.OnConflictFail)
	if err != nil {
		return Applied{}, fmt.Errorf("publish %s: %w", w.Path, err)
	}
	if !protocol.IsWriteSuccess(result.Response.Status) {
		return Applied{}, &Refusal{Path: w.Path, Reason: result.Response.Status}
	}
	version, err := docwrite.Version(result.Response.Metadata)
	if err != nil {
		return Applied{}, errors.New(w.Path + ": " + err.Error())
	}
	maps.Copy(meta, result.Response.Metadata)
	return Applied{Path: w.Path, Action: action, Version: version, Body: w.Body, Metadata: meta}, nil
}
