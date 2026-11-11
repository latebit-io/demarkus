// Package curate brings one document into shape: the style findings on it,
// and the mechanical fix it can propose (the edits the gate's rules imply,
// or a split into a hub plus topic files once it outgrows one fetch). A
// surface shows the proposal and applies it only against its hash.
package curate

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/client/memory/outline"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/docframe"
	"github.com/latebit-io/demarkus/protocol/mdoutline"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// frameCap: a section under it is the document's own frame and stays in
// the hub while the hub fits in one fetch.
const frameCap = 1024

// Input is the document as fetched, and the write policy the server judges
// a split's writes by (nil: none).
type Input struct {
	Path     string
	Body     string
	Version  int
	Metadata map[string]string
	Policy   *publishpolicy.Policy
	Links    links.Resolver // the store's own mark:// links read as paths
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
	// Refused is what a refusing write policy would stop; apply writes
	// nothing while any remains, so a split never lands halfway.
	Refused []string
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
	if len(in.Body) >= outline.Threshold {
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
		p.Fixed = append(p.Fixed, "frontmatter fence removed; metadata keys: "+strings.Join(gate.Lift(kv, meta), ", "))
		headings = mdoutline.Headings(body)
	}
	before := body
	// Headings are renamed on the offsets just parsed, before the em dash
	// pass shifts them.
	body, renamed := stripHeadingStatus(body, headings)
	p.Fixed = append(p.Fixed, renamed...)
	if total := gate.EmDashCount(body); total > 0 {
		var n int
		body, n = replaceEmDashes(body)
		if n > 0 {
			p.Fixed = append(p.Fixed, fmt.Sprintf("%d of %d em dash(es) replaced with a comma; the rest are in code, markup, or open a line", n, total))
		}
	}
	after := mdoutline.Headings(body)
	var summary string
	if desc := strings.TrimSpace(meta["description"]); desc != "" && gate.MissingSummary(body, after) && mdoutline.IsParagraph(desc) {
		body, summary = withSummary(body, after, desc), desc
		after = mdoutline.Headings(body)
		p.Fixed = append(p.Fixed, "metadata.description added as the summary under the H1")
	}
	if body == in.Body {
		return
	}
	// Heading i before is heading i after, so any shifted anchor (a rename,
	// an em dash, a renumbered twin) is a move the document's links follow.
	if !slices.EqualFunc(headings, after, func(a, b mdoutline.Heading) bool { return a.Level == b.Level }) {
		p.Unfixed = append(p.Unfixed, "the edit would add, drop or relevel a heading, so its anchors cannot be mapped; edit by hand")
		return
	}
	homes := landings{headings: headings, at: make(map[string]landing, len(headings))}
	for i, h := range headings {
		homes.at[h.Anchor] = landing{path: in.Path, anchor: after[i].Anchor}
	}
	rl := relocator{r: in.Links, path: in.Path, homes: homes}
	body = rl.rewrite(body, 0, links.Spans(body), in.Path)
	// Whatever form a link takes, it must lead where it did (heading i to
	// heading i), and no text may have become one but the summary's.
	is := landedKeys(in.Links, map[string][]mdoutline.Heading{in.Path: after}, homes)
	want := originalKeys(in.Links, in.Path, headings).count(in.Path, before)
	for k, n := range is.count(in.Path, summary) {
		want[k] += n
	}
	if changed := linkChange(want, is.count(in.Path, body), headings); changed != "" {
		p.Unfixed = append(p.Unfixed, "the edit would change where "+changed+" leads; edit by hand")
		return
	}
	p.recordMoves(in.Path, homes)
	p.Kind = KindEdit
	p.Writes = []Write{{Path: in.Path, Body: body, Metadata: meta}}
	p.Unfixed = gate.AuditProblems(in.Path, body, meta["description"])
}

// recordMoves notes every heading whose landing is not where it was; false
// when none moved.
func (p *Proposal) recordMoves(here string, homes landings) bool {
	for _, h := range homes.headings {
		if to := homes.at[h.Anchor]; to.path != here || to.anchor != h.Anchor {
			p.moved(h.Anchor, to.path+"#"+to.anchor)
		}
	}
	return len(p.Moved) > 0
}

func (p *Proposal) moved(anchor, to string) {
	if p.Moved == nil {
		p.Moved = map[string]string{}
	}
	p.Moved[anchor] = to
}

var (
	parenGroup    = regexp.MustCompile(`\s*\([^()]*\)`)
	lineStartDash = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)]|#{1,6})?\s*—`)
	// Punctuation that sets a status token at an edge apart from the name.
	leadJoint  = regexp.MustCompile(`^(?:\s*[—–:,]\s*|\s+-\s+)`)
	trailJoint = regexp.MustCompile(`(?:\s*[—–:,]\s*|\s+-\s+)$`)
	// A PR number takes its noun with it: "PR #700: x", "Merged: PR #422".
	leadRef  = regexp.MustCompile(`^(?i:PRs?|issues?)\s+#`)
	trailRef = regexp.MustCompile(`(?i)\b(?:PRs?|issues?)\s+$`)
	isoDate  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// functionWords end a phrase a trailing token completes ("merged as PR
// #318", "Week 2026-09-22 to 2026-09-29"), so the token cannot go.
var functionWords = map[string]bool{
	"a": true, "after": true, "and": true, "as": true, "at": true, "before": true, "by": true,
	"for": true, "from": true, "in": true, "into": true, "of": true, "on": true, "or": true,
	"per": true, "since": true, "than": true, "the": true, "through": true, "till": true,
	"to": true, "until": true, "via": true, "with": true,
}

// replaceEmDashes turns em dashes in prose into commas, except at line starts,
// where a dash may be a bullet or open a heading; n is what changed.
func replaceEmDashes(body string) (out string, n int) {
	prose := proseMask{segs: mdoutline.ProseSegments(body)}
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
		line, changed := replaceInProse(body, keep, end, &prose)
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

// proseMask answers whether a byte offset is prose; offsets are asked in order.
type proseMask struct {
	segs []mdoutline.Segment
	i    int
}

func (m *proseMask) at(off int) bool {
	for m.i < len(m.segs) && m.segs[m.i].Stop <= off {
		m.i++
	}
	return m.i < len(m.segs) && off >= m.segs[m.i].Start
}

// replaceInProse rewrites body[from:to] with each em dash in prose replaced
// by ", ", surrounding blanks folded; n is what changed.
func replaceInProse(body string, from, to int, prose *proseMask) (out []byte, n int) {
	out = make([]byte, 0, to-from)
	for i := from; i < to; {
		if !strings.HasPrefix(body[i:to], emDash) || !prose.at(i) {
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

// stripHeadingStatus moves the status tokens of every heading below the H1
// onto a "Status:" line under it; a heading left empty stays as it is.
// Edits run back to front so earlier offsets stay valid.
func stripHeadingStatus(body string, headings []mdoutline.Heading) (out string, renamed []string) {
	for _, h := range slices.Backward(headings) {
		if h.Level == 1 || !gate.CarriesStatus(h.Text) {
			continue
		}
		to, status := withoutStatus(h.Text)
		if to == "" || to == h.Text {
			continue
		}
		// A later heading's edit moved this one's end; only its start is exact.
		end := gate.HeadingEnd(body, h)
		line := body[h.Start:end]
		if !strings.Contains(line, h.Text) {
			continue // inline markup the rename cannot place
		}
		section := strings.Replace(line, h.Text, to, 1) + "\n\nStatus: " + status + "\n\n" + strings.TrimLeft(body[end:], "\n")
		body = body[:h.Start] + section
		renamed = append(renamed, fmt.Sprintf("heading %q is now %q with its status on the line under it", h.Text, to))
	}
	slices.Reverse(renamed)
	return body, renamed
}

// withoutStatus removes a parenthesized group holding a status token whole,
// then tokens at either edge; the removed text is the status line. A token
// inside the sentence ("after #420: x") leaves the heading as it is.
func withoutStatus(text string) (name, status string) {
	var removed []string
	var kept strings.Builder
	last := 0
	for _, m := range parenGroup.FindAllStringIndex(text, -1) {
		inner := strings.TrimSpace(strings.Trim(strings.TrimSpace(text[m[0]:m[1]]), "()"))
		// After a function word the group completes the phrase ("Fix for (#504)").
		if !gate.CarriesStatus(inner) || functionWords[strings.ToLower(lastField(text[:m[0]]))] {
			continue
		}
		kept.WriteString(text[last:m[0]])
		removed, last = append(removed, inner), m[1]
	}
	kept.WriteString(text[last:])
	name = gate.Collapse(kept.String())
	for {
		tok, rest := leadingStatus(name)
		if tok == "" {
			break
		}
		removed, name = append(removed, tok), rest
	}
	for {
		rest, tok := trailingStatus(name)
		if tok == "" {
			break
		}
		removed, name = append(removed, tok), rest
	}
	if gate.CarriesStatus(name) {
		return text, ""
	}
	return strings.Trim(name, " :,-—–"), strings.Join(removed, " ")
}

// leadingStatus cuts a status token opening s when punctuation sets it off,
// or a space after a date ("2026-03-02 Audit"); tok is "" when none does.
func leadingStatus(s string) (tok, rest string) {
	from := 0
	if m := leadRef.FindStringIndex(s); m != nil {
		from = m[1] - 1
	}
	for _, re := range gate.StatusTokens {
		loc := re.FindStringIndex(s[from:])
		if len(loc) == 0 || loc[0] != 0 {
			continue
		}
		end := from + loc[1]
		rest = s[end:]
		switch joint := leadJoint.FindString(rest); {
		case rest == "":
		case joint != "":
			rest = rest[len(joint):]
		case rest[0] == ' ' && isoDate.MatchString(s[from:end]):
			rest = rest[1:]
		default:
			return "", s
		}
		return s[:end], rest
	}
	return "", s
}

// trailingStatus cuts a status token closing s when punctuation or a space
// sets it off, unless the word before needs it; tok is "" when none goes.
func trailingStatus(s string) (rest, tok string) {
	for _, re := range gate.StatusTokens {
		locs := re.FindAllStringIndex(s, -1)
		if len(locs) == 0 || locs[len(locs)-1][1] != len(s) {
			continue
		}
		start := locs[len(locs)-1][0]
		if m := trailRef.FindStringIndex(s[:start]); m != nil && s[start] == '#' {
			start = m[0]
		}
		rest = s[:start]
		switch joint := trailJoint.FindString(rest); {
		case rest == "":
		case joint != "":
			rest = rest[:len(rest)-len(joint)]
		case strings.HasSuffix(rest, " ") && !functionWords[strings.ToLower(lastField(rest))]:
			rest = strings.TrimRight(rest, " ")
		default:
			return s, ""
		}
		return rest, s[start:]
	}
	return s, ""
}

func lastField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// withSummary puts the sentence under the first H1.
func withSummary(body string, headings []mdoutline.Heading, summary string) string {
	for _, h := range headings {
		if h.Level != 1 {
			continue
		}
		end := gate.HeadingEnd(body, h)
		return body[:end] + "\n\n" + summary + "\n\n" + strings.TrimLeft(body[end:], "\n")
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
// <path without .md>/, headings verbatim, and leaves the document as the
// hub: its preamble, a links list, and its short sections.
func split(in *Input, headings []mdoutline.Heading, p *Proposal) {
	sections, refusal := planSections(in, headings)
	if refusal != "" {
		p.Unfixed = append(p.Unfixed, refusal)
		return
	}
	// Short sections stay only while the hub fits in one fetch: first by the
	// raw text, then with links rebased, since a rebased link is longer.
	for len(hubBody(preambleOf(in.Body, sections), sections, headings)) >= outline.Threshold {
		if !moveLargest(sections) {
			break
		}
	}
	title := hub.Title(in.Body, in.Metadata, strings.TrimSuffix(path.Base(in.Path), ".md"))
	var homes landings
	var preamble, hubDefs string
	var plan []section
	orig := linksOf(in)
	for {
		if homes, refusal = anchorHomes(in, title, sections, headings); refusal != "" {
			p.Unfixed = append(p.Unfixed, refusal)
			return
		}
		preamble, plan, hubDefs = orig.rebased(sections, homes)
		if len(hubBody(preamble, plan, headings)+hubDefs) < outline.Threshold || !moveLargest(sections) {
			break
		}
	}
	carried := docwrite.Carried(in.Metadata)
	var writes []Write
	var fixed []string
	for i := range plan {
		if s := &plan[i]; s.topic != "" {
			w := topicWrite(in, title, carried, s)
			writes = append(writes, w)
			fixed = append(fixed, fmt.Sprintf("%q moved to %s (%d bytes)", s.heading.Text, s.topic, len(w.Body)))
		}
	}
	body := hubBody(preamble, plan, headings) + hubDefs
	meta := maps.Clone(carried)
	meta["tags"] = strings.Join(hubTags(preamble, plan, carried["tags"]), ",")
	writes = append(writes, Write{Path: in.Path, Body: body, Metadata: meta})
	if lost := orig.lostLink(sections, writes, homes); lost != "" {
		p.Unfixed = append(p.Unfixed, "a split would change where "+lost+" leads; split by hand")
		return
	}
	p.recordMoves(in.Path, homes)
	p.Writes = writes
	p.Fixed = append(p.Fixed, fixed...)
	p.Fixed = append(p.Fixed, fmt.Sprintf("%s becomes the hub (%d bytes)", in.Path, len(body)))
	p.Kind = KindSplit
	for _, w := range writes {
		p.policyGaps(w.Path, w.Metadata, w.Body, in.Policy)
	}
	p.Unfixed = append(p.Unfixed, gate.AuditProblems(in.Path, body, meta["description"])...)
}

// moveLargest sends the largest section still in the hub to its topic file;
// false when every section has moved.
func moveLargest(sections []section) bool {
	largest := -1
	for i := range sections {
		if sections[i].topic == "" && (largest < 0 || len(sections[i].text) > len(sections[largest].text)) {
			largest = i
		}
	}
	if largest < 0 {
		return false
	}
	sections[largest].topic = sections[largest].candidate
	return true
}

// docLinks is the original's links, parsed once for every split pass.
type docLinks struct {
	in      *Input
	spans   []links.Span
	defs    []links.Definition
	targets []links.Followed
}

func linksOf(in *Input) *docLinks {
	return &docLinks{in: in, spans: links.Spans(in.Body), defs: links.Definitions(in.Body), targets: links.Targets(in.Body)}
}

// rebased writes every link of the preamble and sections for the file it
// lands in, and gives each file the definitions its uses need (the hub's as
// hubDefs). Parsed whole: a slice alone can read differently.
func (o *docLinks) rebased(sections []section, homes landings) (preamble string, plan []section, hubDefs string) {
	in := o.in
	rl := relocator{r: in.Links, path: in.Path, homes: homes}
	defs := o.missingDefinitions(sections)
	preamble = rl.rewrite(preambleOf(in.Body, sections), 0, o.spans, in.Path)
	plan = slices.Clone(sections)
	for i := range plan {
		s := &plan[i]
		s.text = rl.rewrite(s.text, s.heading.Start, o.spans, cmp.Or(s.topic, in.Path))
		if s.topic != "" && defs[s.topic] != nil {
			s.text = strings.TrimRight(s.text, "\n") + "\n\n" + rl.definitions(defs[s.topic], s.topic)
		}
		// The bullet quotes the section's opening line into the hub.
		s.bullet = rl.rewrite(s.bullet, 0, links.Spans(s.bullet), in.Path)
	}
	if defs[in.Path] != nil {
		hubDefs = "\n" + rl.definitions(defs[in.Path], in.Path)
	}
	return preamble, plan, hubDefs
}

// missingDefinitions is, per file, the reference definitions its uses need
// but the text it gets does not hold, a label's first definition winning.
func (o *docLinks) missingDefinitions(sections []section) map[string][]*links.Definition {
	type held struct{ file, label string }
	first := map[string]*links.Definition{}
	has := map[held]bool{}
	for i := range o.defs {
		d := &o.defs[i]
		label := links.LabelKey(d.Label)
		if first[label] == nil {
			first[label] = d
		}
		has[held{landsIn(o.in.Path, sections, d.Pos), label}] = true
	}
	out := map[string][]*links.Definition{}
	for _, r := range o.routes(sections) {
		if r.Label == "" {
			continue
		}
		k := held{r.file, links.LabelKey(r.Label)}
		if d := first[k.label]; d != nil && !has[k] {
			has[k] = true
			out[r.file] = append(out[r.file], d)
		}
	}
	return out
}

// route is a link of the original and the file a reader follows it from.
type route struct {
	file string
	links.Followed
}

// routes is every link of the original from where its text lands, plus the
// moved sections' hub bullets read as in the original, every label it
// defines resolving.
func (o *docLinks) routes(sections []section) []route {
	out := make([]route, 0, len(o.targets))
	for _, f := range o.targets {
		out = append(out, route{landsIn(o.in.Path, sections, f.Pos), f})
	}
	var b strings.Builder
	for i := range sections {
		if sections[i].topic != "" {
			b.WriteString(sections[i].bullet + "\n")
		}
	}
	if b.Len() == 0 {
		return out
	}
	for i := range o.defs {
		b.WriteString("\n" + o.defs[i].Text + "\n")
	}
	for _, f := range links.Targets(b.String()) {
		out = append(out, route{o.in.Path, f})
	}
	return out
}

// landsIn is the file the original's byte at pos goes to.
func landsIn(docPath string, sections []section, pos int) string {
	for i := range slices.Backward(sections) {
		if pos >= sections[i].heading.Start {
			return cmp.Or(sections[i].topic, docPath)
		}
	}
	return docPath
}

// lostLink names a link of the original that, from the file its text lands
// in, leads elsewhere or nowhere; "" when none does. Links the split adds
// (the topics list, a topic's link back) are not the original's.
func (o *docLinks) lostLink(sections []section, writes []Write, homes landings) string {
	type at struct {
		file string
		t    target
	}
	in, headings := o.in, homes.headings
	was := originalKeys(in.Links, in.Path, headings)
	want := map[at]int{}
	for _, r := range o.routes(sections) {
		want[at{r.file, was.of(in.Path, r.Dest)}]++
	}
	files := make(map[string][]mdoutline.Heading, len(writes))
	for _, w := range writes {
		files[w.Path] = mdoutline.Headings(w.Body)
	}
	is := landedKeys(in.Links, files, homes)
	got := map[at]int{}
	for _, w := range writes {
		for t, n := range is.count(w.Path, w.Body) {
			got[at{w.Path, t}] = n
		}
	}
	var lost []string
	for k, n := range want {
		if got[k] < n {
			lost = append(lost, "a link to "+k.t.name(headings)+" in "+k.file)
		}
	}
	if len(lost) == 0 {
		return ""
	}
	return slices.Min(lost)
}

// relocator writes the original's link destinations for the file a piece of
// it lands in.
type relocator struct {
	r     links.Resolver
	path  string // the original document
	homes landings
}

// relocate is dest, as written in the original, written from file here: a
// self link at its heading's landing (the link's own form kept where it
// still names here), a relative path at what it named; anything else as is.
func (rl *relocator) relocate(dest, here string) string {
	doc, _, _ := strings.Cut(dest, "#")
	frag := dest[len(doc):] // with its '#'
	bare, _, _ := strings.Cut(doc, "?")
	query := doc[len(bare):]
	if !rl.r.Self(rl.path, doc) {
		abs := links.ResolvePath(rl.path, doc)
		if here == rl.path || abs == "" || strings.HasPrefix(doc, "/") {
			return dest
		}
		return abs + query + frag
	}
	if frag != "" {
		if from, to, ok := rl.homes.find(frag[1:]); ok {
			if to.path != here || (doc != "" && here != rl.path) {
				return to.path + query + "#" + to.anchor
			}
			if to.anchor == from {
				return dest
			}
			return doc + "#" + to.anchor
		}
	}
	if here == rl.path {
		return dest
	}
	return rl.path + query + frag
}

// rewrite relocates the spans inside text, which starts at base in the
// original and lands in file here.
func (rl *relocator) rewrite(text string, base int, spans []links.Span, here string) string {
	var b strings.Builder
	last := 0
	for i := range spans {
		sp := &spans[i]
		// Spans are in order and apart; one overlapping the last written
		// would slice backwards.
		if sp.Start < base+last || sp.End > base+len(text) {
			continue
		}
		if to := rl.relocate(sp.Dest, here); to != sp.Dest {
			b.WriteString(text[last : sp.Start-base])
			b.WriteString(to)
			last = sp.End - base
		}
	}
	b.WriteString(text[last:])
	return b.String()
}

// definitions copies defs as written for file here, one per line.
func (rl *relocator) definitions(defs []*links.Definition, here string) string {
	var b strings.Builder
	for _, d := range defs {
		b.WriteString(rl.rewrite(d.Text, 0, links.Spans(d.Text), here) + "\n")
	}
	return b.String()
}

// linkKeys names where links lead, so one destination compares equal across
// a rewrite: a heading of the original by index, else its path or URL.
type linkKeys struct {
	r      links.Resolver
	files  map[string][]mdoutline.Heading // each file's headings as written
	origin map[landing]int                // a written heading's index in the original
}

// originalKeys reads links in the document as it was.
func originalKeys(r links.Resolver, docPath string, headings []mdoutline.Heading) *linkKeys {
	k := &linkKeys{r: r, files: map[string][]mdoutline.Heading{docPath: headings}, origin: make(map[landing]int, len(headings))}
	for i, h := range headings {
		k.origin[landing{docPath, h.Anchor}] = i
	}
	return k
}

// landedKeys reads links in the files written, tracing each heading back
// through homes.
func landedKeys(r links.Resolver, files map[string][]mdoutline.Heading, homes landings) *linkKeys {
	k := &linkKeys{r: r, files: files, origin: make(map[landing]int, len(homes.headings))}
	for i, h := range homes.headings {
		k.origin[homes.at[h.Anchor]] = i
	}
	return k
}

// of is where dest, a link in file from, leads.
func (k *linkKeys) of(from, dest string) target {
	doc, frag, hasFrag := strings.Cut(dest, "#")
	to := from
	if !k.r.Self(from, doc) {
		if to = k.r.Path(from, doc); to == "" {
			return target{dest: dest, heading: -1}
		}
	}
	// A fragment naming no heading of the original (the topics list a split
	// adds) keys by its destination as written.
	if hs, ok := k.files[to]; ok && hasFrag {
		if h, found := mdoutline.Find(hs, frag); found {
			if i, known := k.origin[landing{to, h.Anchor}]; known {
				return target{heading: i}
			}
		}
	}
	if hasFrag {
		to += "#" + frag
	}
	return target{dest: to, heading: -1}
}

// count tallies where body's links, written in file from, lead.
func (k *linkKeys) count(from, body string) map[target]int {
	out := map[target]int{}
	for _, f := range links.Targets(body) {
		out[k.of(from, f.Dest)]++
	}
	return out
}

// target is where a link leads: a heading of the document by index, so a
// renamed anchor that still lands compares equal; else its destination.
type target struct {
	dest    string
	heading int
}

// linkChange names a link whose count differs between want and got, the
// least by name so the report is stable; "" when none does.
func linkChange(want, got map[target]int, headings []mdoutline.Heading) string {
	var changed []string
	for k, n := range want {
		if got[k] != n {
			changed = append(changed, k.name(headings))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			changed = append(changed, k.name(headings))
		}
	}
	if len(changed) == 0 {
		return ""
	}
	return "a link to " + slices.Min(changed)
}

func (t target) name(headings []mdoutline.Heading) string {
	if t.heading >= 0 {
		return "#" + headings[t.heading].Anchor
	}
	return t.dest
}

// landing is a heading's file and anchor after the split.
type landing struct{ path, anchor string }

// landings maps the document's anchors to where the split lands them.
type landings struct {
	headings []mdoutline.Heading
	at       map[string]landing // by original anchor
}

// find resolves fragment as fetch does (mdoutline.Find: any case, or its
// slug), returning the anchor it names and where that lands.
func (l landings) find(fragment string) (anchor string, to landing, ok bool) {
	h, ok := mdoutline.Find(l.headings, fragment)
	if !ok {
		return "", landing{}, false
	}
	to, ok = l.at[h.Anchor]
	return h.Anchor, to, ok
}

// anchorHomes maps each anchor to its landing, parsed from the bodies the
// split writes (each file renumbers duplicate slugs). A heading that does not
// come back from that parse is a refusal: its text or links would be lost.
func anchorHomes(in *Input, title string, sections []section, headings []mdoutline.Heading) (homes landings, refusal string) {
	homes = landings{headings: headings, at: make(map[string]landing, len(headings))}
	// place zips a file's original headings with its parsed ones; a nil entry
	// is a heading the split adds (the topic H1, the hub's links list).
	place := func(file, body string, orig []*mdoutline.Heading) {
		parsed := mdoutline.Headings(body)
		for i, h := range orig {
			if h == nil {
				continue
			}
			if i >= len(parsed) || len(parsed) != len(orig) || parsed[i].Level != h.Level || parsed[i].Text != h.Text {
				refusal = cmp.Or(refusal, fmt.Sprintf("heading %q would not survive the split into %s (nested in a list or block?); make it a top-level heading first", h.Text, file))
				return
			}
			homes.at[h.Anchor] = landing{file, parsed[i].Anchor}
		}
	}
	within := func(s *section) []*mdoutline.Heading {
		var span []*mdoutline.Heading
		for i := range headings {
			if h := &headings[i]; h.Start >= s.heading.Start && h.Start < s.heading.End {
				span = append(span, h)
			}
		}
		return span
	}
	var hubHeads []*mdoutline.Heading
	for i := range headings {
		if headings[i].Start < sections[0].heading.Start {
			hubHeads = append(hubHeads, &headings[i])
		}
	}
	hubHeads = append(hubHeads, nil)
	for i := range sections {
		s := &sections[i]
		if s.topic == "" {
			hubHeads = append(hubHeads, within(s)...)
			continue
		}
		place(s.topic, topicBody(title, in.Path, s), append([]*mdoutline.Heading{nil}, within(s)...))
	}
	place(in.Path, hubBody(preambleOf(in.Body, sections), sections, headings), hubHeads)
	for _, h := range headings {
		if _, ok := homes.at[h.Anchor]; !ok && refusal == "" {
			refusal = fmt.Sprintf("heading %q is outside every second-level section (a second H1?), so a split would drop it", h.Text)
		}
	}
	return homes, refusal
}

// policyGaps names what the write policy finds missing on a write, as the
// server would judge it: warn lets it land with a note, block and ask refuse.
func (p *Proposal) policyGaps(docPath string, meta map[string]string, body string, policy *publishpolicy.Policy) {
	verb, refuses := "warns on it: ", false
	if policy != nil && policy.EffectiveStrictness() != publishpolicy.Warn {
		verb, refuses = "refuses it: ", true
	}
	for _, m := range gate.Missing(docPath, meta, body, policy) {
		line := docPath + ": the write policy " + verb + m
		p.Unfixed = append(p.Unfixed, line)
		if refuses {
			p.Refused = append(p.Refused, line)
		}
	}
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

// preambleOf is what precedes the first section: the hub keeps it.
func preambleOf(body string, sections []section) string {
	return strings.TrimRight(body[:sections[0].heading.Start], "\n")
}

func topicSummary(title string, s *section) string {
	return fmt.Sprintf("The %s section of %s, split into its own document.", s.heading.Text, title)
}

func topicBody(title, parentPath string, s *section) string {
	return fmt.Sprintf("# %s: %s\n\n%s\n\nPart of [%s](%s).\n\n%s\n", title, s.heading.Text, topicSummary(title, s), title, parentPath, strings.TrimRight(s.text, "\n"))
}

// topicWrite is one moved section as its own document: derived H1 and
// summary, a link back, the section verbatim. Every parent axis tag and
// policy-required field carries, so a topic meets its parent's policy.
func topicWrite(in *Input, title string, carried map[string]string, s *section) Write {
	parentPath, policy := in.Path, in.Policy
	tags := protocol.MergeTags([]string{strings.TrimSuffix(path.Base(parentPath), ".md"), mdoutline.Slug(s.heading.Text)}, nil)
	lower := strings.ToLower(s.text)
	for _, tag := range protocol.SplitTags(carried["tags"]) {
		if strings.Contains(tag, ":") || mentions(lower, tag) {
			tags = protocol.MergeTags(tags, []string{tag})
		}
	}
	summary := topicSummary(title, s)
	meta := map[string]string{"tags": strings.Join(tags, ","), "importance": topicImportance(carried["importance"]), "description": summary, neardup.CuratedFromKey: parentPath}
	if t := carried["type"]; t != "" {
		meta["type"] = t
	}
	// The server types an untyped topic on write; typing it here keeps the
	// digest equal to what lands, so a resumed split finds it identical.
	meta = storefmt.ApplyOKFTypeDefault(s.topic, meta)
	if policy != nil {
		for _, field := range policy.ForCaller().RequiredFields {
			if _, set := meta[field]; !set && carried[field] != "" {
				meta[field] = carried[field]
			}
		}
	}
	body := topicBody(title, parentPath, s)
	meta[curatedDigest] = topicDigest(body, meta)
	return Write{Path: s.topic, Body: body, Metadata: meta}
}

// hubTags drops a tag the hub's own prose never mentions when a topic does;
// the links list naming a topic is not a mention. Axis tags always stay,
// since a policy may require them.
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
		if strings.Contains(tag, ":") || mentions(ownLower, tag) || !mentions(movedLower, tag) {
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
	for _, link := range outline.AnchoredLinks(src.Body) {
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
	// Matched as fetch resolves a fragment, so "#NEXT-2" finds next-2.
	moved := make([]mdoutline.Heading, 0, len(p.Moved))
	for anchor := range p.Moved {
		moved = append(moved, mdoutline.Heading{Anchor: anchor})
	}
	var moves []Move
	for _, link := range inbound {
		if h, ok := mdoutline.Find(moved, link.Anchor); ok {
			moves = append(moves, Move{Source: link.Source, From: p.Path + "#" + link.Anchor, To: p.Moved[h.Anchor]})
		}
	}
	return moves
}

// Report renders the proposal for the agent to show: the findings, the
// change, what is left, the links to repoint, then every body in full.
func (p *Proposal) Report(moves []Move, notes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\npath: %s\nversion: %d\nsize: %d bytes\n", p.Kind, headerValue(p.Path), p.Version, p.Size)
	// In the header, above any body the proposal quotes, so document text
	// cannot pose as it.
	if len(p.Writes) > 0 {
		b.WriteString(p.approval(moves, notes) + "\n")
	}
	b.WriteString("\n## Checks\n\n")
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
		// Document text goes out framed and flagged, as every other read does.
		fmt.Fprintf(&b, "\n=== %s (%s)\n%s%s", headerValue(w.Path), metaLine(w.Metadata), docframe.Frame(w.Body), docframe.Flags(w.Body, w.Metadata))
	}
	return b.String()
}

// approval says whether applying needs a human: an edit of one document with
// no inbound link to repoint, and nothing unknown about them, does not.
func (p *Proposal) approval(moves []Move, notes []string) string {
	if p.Kind == KindEdit && len(moves) == 0 && len(notes) == 0 {
		return "approval: none"
	}
	return "approval: required"
}

// headerValue keeps a field on its header line: a value with a newline or
// any other non-printing rune is quoted, so it cannot add a line the gate reads.
func headerValue(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return strconv.Quote(s)
	}
	return s
}

func metaLine(meta map[string]string) string {
	parts := make([]string, 0, len(meta))
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		parts = append(parts, k+": "+headerValue(meta[k]))
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
	if len(p.Refused) > 0 {
		return nil, &Refusal{Path: p.Path, Reason: strings.Join(p.Refused, "; ") + "; nothing written"}
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
		case h.Metadata[neardup.CuratedFromKey] == p.Path && h.Metadata[curatedDigest] == topicDigest(h.Body, docwrite.Carried(h.Metadata)) && h.Version > 0:
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
		return Applied{}, &Refusal{Path: w.Path, Reason: result.Response.Refusal()}
	}
	version, err := docwrite.Version(result.Response.Metadata)
	if err != nil {
		return Applied{}, errors.New(w.Path + ": " + err.Error())
	}
	maps.Copy(meta, result.Response.Metadata)
	return Applied{Path: w.Path, Action: action, Version: version, Body: w.Body, Metadata: meta}, nil
}
