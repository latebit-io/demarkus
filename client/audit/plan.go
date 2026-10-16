package audit

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/curate"
	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/docwrite/gate"
	"github.com/latebit-io/demarkus/client/hub"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
)

// MaxPlanWrites bounds one apply; a rerun plans the rest.
const MaxPlanWrites = 100

// generatedHubImportance ranks a section hub under the root and project hubs.
const generatedHubImportance = "0.6"

// Plan is what apply writes: every fix on one document composed into one
// write at the version the audit read, the batch bound by Hash. It is built
// from the audit pass alone, with no further reads.
type Plan struct {
	Writes  []PlanWrite
	Unfixed []Finding // leads the plan cannot settle, and why
	More    int       // writes past MaxPlanWrites, left to a rerun
	Hash    string
}

// PlanWrite is one document's composed fix.
type PlanWrite struct {
	Path     string
	Version  int // the version read; 0 creates
	Body     string
	Metadata map[string]string
	Requires []string // writes that must land first: the hub a parent's bullets move to
	Fixes    []string // one line per change
}

// leads are the findings a fix may settle, recorded by the check that filed
// them with the finding's index.
type leads struct {
	archived                                []archivedLead
	adr                                     []adrLead
	orphans, hubs, frontmatter, journalType []lead
}

type lead struct {
	path    string
	finding int
}

type archivedLead struct {
	doc, target string
	findings    []int // the broken link, and on a hub the stale entry
}

type adrLead struct {
	lead
	num, record string
}

// draft is one document's pending write.
type draft struct {
	version  int
	body     string
	meta     map[string]string
	requires []string
	fixes    []string
	changed  bool
}

func (d *draft) change(body, fix string) {
	d.body, d.changed = body, true
	d.fixes = append(d.fixes, fix)
}

type planner struct {
	a       *audit
	drafts  map[string]*draft
	unfixed []Finding
}

// plan composes the fixes the leads prove. Hubs are generated before
// orphans are placed, so an orphan sees the new hub and the room it frees.
func (a *audit) plan() *Plan {
	pl := &planner{a: a, drafts: map[string]*draft{}}
	pl.frontmatter()
	pl.journalType()
	pl.archived()
	pl.adrLinks()
	pl.hubs()
	pl.orphans()
	return pl.finish()
}

// draft opens p's pending write at the version the audit read; nil when p
// is not a live document the audit read with its version.
func (pl *planner) draft(p string) *draft {
	if d, ok := pl.drafts[p]; ok {
		return d
	}
	doc, ok := pl.a.docs[p]
	if !ok || !live(doc.status) {
		return nil
	}
	if v, err := docwrite.Version(doc.meta); err == nil && v > 0 {
		pl.drafts[p] = &draft{version: v, body: doc.body, meta: docwrite.Carried(doc.meta)}
		return pl.drafts[p]
	}
	return nil
}

// current is p's body and metadata with the plan's changes so far.
func (pl *planner) current(p string) (body string, meta map[string]string) {
	if d, ok := pl.drafts[p]; ok {
		return d.body, d.meta
	}
	return pl.a.docs[p].body, pl.a.docs[p].meta
}

// exists: a live document of the inventory, or a hub the plan creates.
func (pl *planner) exists(p string) bool {
	if d, ok := pl.drafts[p]; ok && d.version == 0 {
		return true
	}
	doc, ok := pl.a.docs[p]
	return ok && live(doc.status)
}

// nearestHub is the first of p's ancestor hubs that exists, or assumed
// when it comes first; "" when none does.
func (pl *planner) nearestHub(p, assumed string) string {
	for _, h := range hub.Ancestors(p) {
		if h == assumed || pl.exists(h) {
			return h
		}
	}
	return ""
}

func (pl *planner) settle(findings ...int) {
	for _, i := range findings {
		pl.a.findings[i].Fixable = true
	}
}

func (pl *planner) skip(check, p, why string) {
	pl.unfixed = append(pl.unfixed, Finding{Check: check, Path: p, Detail: why})
}

func (pl *planner) noDraft(check, p string) {
	pl.skip(check, p, p+" was not read with a version; rerun the audit")
}

func (pl *planner) frontmatter() {
	for _, l := range pl.a.leads.frontmatter {
		d := pl.draft(l.path)
		if d == nil {
			pl.noDraft(CheckFrontmatter, l.path)
			continue
		}
		kv, rest, ok := gate.Frontmatter(d.body)
		if !ok {
			pl.skip(CheckFrontmatter, l.path, "the fence does not parse as key: value lines; strip it by hand")
			continue
		}
		keys := curate.Lift(kv, d.meta)
		if len(keys) == 0 {
			keys = []string{"none"}
		}
		d.change(rest, "frontmatter fence removed; metadata keys: "+strings.Join(keys, ", "))
		pl.settle(l.finding)
	}
}

// isJournalEntry: a dated entry directly in a journal directory.
func (a *audit) isJournalEntry(p string) bool {
	return layout.IsJournalEntry(path.Base(p)) && path.Base(path.Dir(p))+"/" == a.layout.Journal
}

func (pl *planner) journalType() {
	for _, l := range pl.a.leads.journalType {
		d := pl.draft(l.path)
		if d == nil {
			pl.noDraft(CheckUntyped, l.path)
			continue
		}
		d.meta["type"] = "Journal"
		d.change(d.body, "type Journal")
		pl.settle(l.finding)
	}
}

// archived drops a hub's bullets that link only archived documents, then
// unlinks the rest inline, keeping the text.
func (pl *planner) archived() {
	byDoc := map[string][]archivedLead{}
	for _, l := range pl.a.leads.archived {
		byDoc[l.doc] = append(byDoc[l.doc], l)
	}
	for _, doc := range slices.Sorted(maps.Keys(byDoc)) {
		found := byDoc[doc]
		d := pl.draft(doc)
		if d == nil {
			pl.noDraft(CheckBrokenLinks, doc)
			continue
		}
		targets := map[string]bool{}
		for _, l := range found {
			targets[l.target] = true
		}
		body, bullets := d.body, 0
		if gate.IsHub(doc, body) || pl.a.dirs[strings.TrimSuffix(doc, ".md")+"/"] {
			body, bullets = hub.WithoutLinks(body, doc, slices.Sorted(maps.Keys(targets)))
		}
		body, inline := unlinkInline(body, doc, targets)
		left := map[string]bool{}
		for _, dest := range links.Extract(body) {
			left[links.ResolvePath(doc, dest)] = true
		}
		for _, l := range found {
			if left[l.target] {
				pl.skip(CheckBrokenLinks, doc, "-> "+l.target+" is not an inline link; remove it by hand")
				continue
			}
			pl.settle(l.findings...)
		}
		if body != d.body {
			d.change(body, fmt.Sprintf("links to archived documents removed: %d hub bullet(s), %d inline with the text kept", bullets, inline))
		}
	}
}

// unlinkInline replaces each inline link to a target with its text; n
// counts them. A reference-style link has no inline destination and stays.
func unlinkInline(body, docPath string, targets map[string]bool) (out string, n int) {
	type cut struct{ start, end, textStart, textEnd int }
	var cuts []cut
	for _, l := range links.ExtractWithPositions(body) {
		if l.OpenBracket < 0 || !targets[links.ResolvePath(docPath, l.Dest)] {
			continue
		}
		if end := inlineEnd(body, l.CloseBracket); end > 0 {
			cuts = append(cuts, cut{l.OpenBracket, end, l.OpenBracket + 1, l.CloseBracket})
		}
	}
	for i := len(cuts) - 1; i >= 0; i-- {
		c := cuts[i]
		body = body[:c.start] + body[c.textStart:c.textEnd] + body[c.end:]
	}
	return body, len(cuts)
}

// inlineEnd is the offset just past an inline link's "(dest)" that starts
// after the ']' at closeBracket; -1 when none does.
func inlineEnd(body string, closeBracket int) int {
	i := closeBracket + 1
	if i >= len(body) || body[i] != '(' {
		return -1
	}
	depth := 0
	for ; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '\n':
			return -1
		}
	}
	return -1
}

func (pl *planner) adrLinks() {
	byDoc := map[string][]adrLead{}
	for _, l := range pl.a.leads.adr {
		byDoc[l.path] = append(byDoc[l.path], l)
	}
	for _, doc := range slices.Sorted(maps.Keys(byDoc)) {
		found := byDoc[doc]
		d := pl.draft(doc)
		if d == nil {
			pl.noDraft(CheckReferences, doc)
			continue
		}
		want := map[string]string{}
		for _, l := range found {
			want[l.num] = l.record
		}
		body, linked := linkADRs(d.body, want)
		var nums []string
		for _, l := range found {
			if !linked[l.num] {
				pl.skip(CheckReferences, doc, "ADR "+l.num+" is mentioned only in code, links or headings; link it by hand")
				continue
			}
			nums = append(nums, l.num)
			pl.settle(l.finding)
		}
		if len(nums) > 0 {
			d.change(body, "linked ADR "+strings.Join(nums, ", "))
		}
	}
}

// linkADRs links the first mention of each wanted number (number to
// record) outside code, links, headings and ranges.
func linkADRs(body string, want map[string]string) (out string, linked map[string]bool) {
	type span struct{ start, end int }
	var skip []span
	for _, l := range links.ExtractWithPositions(body) {
		if l.OpenBracket >= 0 {
			skip = append(skip, span{l.OpenBracket, max(inlineEnd(body, l.CloseBracket), l.CloseBracket+1)})
		}
	}
	for _, h := range mdoutline.Headings(body) {
		end := len(body)
		if i := strings.IndexByte(body[h.Start:], '\n'); i >= 0 {
			end = h.Start + i
		}
		skip = append(skip, span{h.Start, end})
	}
	inside := func(off int) bool {
		return slices.ContainsFunc(skip, func(s span) bool { return off >= s.start && off < s.end })
	}
	type link struct {
		start, end int
		record     string
	}
	var adds []link
	linked = map[string]bool{}
	masked := stripCode(body)
	for _, m := range adrReferenceRe.FindAllStringSubmatchIndex(masked, -1) {
		digits := masked[m[2]:m[3]] // three or four, as the check pads them
		num := strings.Repeat("0", adrNumberWidth-len(digits)) + digits
		record, ok := want[num]
		if m[4] >= 0 || !ok || linked[num] || inside(m[0]) {
			continue
		}
		linked[num] = true
		adds = append(adds, link{m[0], m[1], record})
	}
	for i := len(adds) - 1; i >= 0; i-- {
		l := adds[i]
		body = body[:l.start] + "[" + body[l.start:l.end] + "](" + l.record + ")" + body[l.end:]
	}
	return body, linked
}

// hubs generates each missing hub at D/index.md: the parent's bullets that
// link only documents in D/ move down, the parent keeps one bullet for D/,
// and every document the new hub is nearest to gets a bullet.
func (pl *planner) hubs() {
	for _, l := range pl.a.leads.hubs {
		dir := l.path
		if path.Base(dir)+"/" == pl.a.layout.Journal {
			pl.skip(CheckMissingHub, dir, "a journal holds dated entries; rename the rest to <date>.md or move them out")
			continue
		}
		hubPath := hub.Paths(dir)[0]
		parentPath := pl.nearestHub(hubPath, "")
		parent := pl.draft(parentPath)
		if parent == nil {
			pl.skip(CheckMissingHub, dir, "no parent hub in scope to link "+dir+" from")
			continue
		}
		title := layout.Section(path.Base(dir) + "/")
		summary := title + " documents, one link each."
		var inDir []string
		for _, q := range pl.a.order {
			if strings.HasPrefix(q, dir) && pl.exists(q) {
				inDir = append(inDir, q)
			}
		}
		// A parent that links D/ itself keeps that bullet; else the first
		// moved bullet's line becomes the one for D/.
		dirBullet := fmt.Sprintf("- [%s](%s): %s", title, dir, summary)
		linked := linksDir(parent.body, parentPath, dir)
		replace := dirBullet
		if linked {
			replace = ""
		}
		parentBody, moved := hub.MoveLinks(parent.body, parentPath, inDir, replace)
		if !linked && len(moved) == 0 {
			parentBody = hub.WithBullet(parentBody, "", dirBullet)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n%s\n\n", title, summary)
		for _, line := range moved {
			b.WriteString(strings.TrimLeft(line, " \t") + "\n")
		}
		body, added := b.String(), 0
		for _, q := range inDir {
			if !hub.Linkable(q) || pl.nearestHub(q, hubPath) != hubPath || hub.Names(body, hubPath, q) {
				continue
			}
			qBody, qMeta := pl.current(q)
			body += hub.Bullet(q, qBody, qMeta) + "\n"
			added++
		}
		if n := gate.OutboundDocs(body); n > gate.HubLinkMax {
			pl.skip(CheckMissingHub, dir, fmt.Sprintf("over the cap: one hub would link %d documents, %d is the cap; publish %s as its hub or add second-level hubs", n, gate.HubLinkMax, hub.Paths(dir)[1]))
			continue
		}
		pl.drafts[hubPath] = &draft{
			body:    body,
			meta:    map[string]string{"tags": strings.Join(protocol.MergeTags([]string{"index"}, []string{path.Base(dir)}), ","), "importance": generatedHubImportance},
			fixes:   []string{fmt.Sprintf("hub created: %d bullet(s) moved from %s, %d added", len(moved), parentPath, added)},
			changed: true,
		}
		parent.change(parentBody, fmt.Sprintf("%d bullet(s) moved to %s, one bullet for %s", len(moved), hubPath, dir))
		parent.requires = append(parent.requires, hubPath)
		pl.settle(l.finding)
	}
}

// linksDir: the body links the directory itself, slash optional.
func linksDir(body, from, dir string) bool {
	for _, dest := range links.Extract(body) {
		if r := links.ResolvePath(from, dest); r == dir || r == strings.TrimSuffix(dir, "/") {
			return true
		}
	}
	return false
}

// orphans links each orphan from its nearest hub by the directory rule; a
// dated journal entry is reached by a link to its directory instead.
func (pl *planner) orphans() {
	for _, l := range pl.a.leads.orphans {
		switch p := l.path; {
		case pl.a.isJournalEntry(p):
			project := path.Dir(path.Dir(p))
			journal := strings.TrimSuffix(project, "/") + "/" + pl.a.layout.Journal
			section, bullet := hub.JournalBullet(pl.a.layout.Journal)
			pl.link(l, hub.Paths(project)[0], &hub.Link{Target: journal, Section: section, Bullet: bullet})
		case !hub.Linkable(p):
			pl.skip(CheckOrphans, p, "not a document a hub names; link its directory from the hub above it")
		default:
			hubPath := pl.nearestHub(p, "")
			if hubPath == "" {
				pl.skip(CheckOrphans, p, "no hub in scope by the directory rule")
				continue
			}
			body, meta := pl.current(p)
			pl.link(l, hubPath, &hub.Link{Target: p, Bullet: hub.Bullet(p, body, meta)})
		}
	}
}

// link adds the bullet for want's target (the orphan or its directory) to
// the hub unless the hub already names the orphan, and settles it.
func (pl *planner) link(l lead, hubPath string, want *hub.Link) {
	d := pl.draft(hubPath)
	if d == nil {
		pl.skip(CheckOrphans, l.path, "its hub "+hubPath+" is not in scope")
		return
	}
	switch {
	case hub.Names(d.body, hubPath, l.path):
	case gate.OutboundDocs(d.body) >= gate.HubLinkMax:
		pl.skip(CheckOrphans, l.path, fmt.Sprintf("its hub %s is at the cap of %d links", hubPath, gate.HubLinkMax))
		return
	default:
		d.change(hub.WithBullet(d.body, want.Section, want.Bullet), "bullet for "+want.Target)
	}
	pl.settle(l.finding)
}

// finish orders the writes (created hubs, then the parents that need them,
// then by path), cuts the batch at MaxPlanWrites and hashes it.
func (pl *planner) finish() *Plan {
	p := &Plan{Unfixed: pl.unfixed}
	for docPath, d := range pl.drafts {
		if d.changed {
			p.Writes = append(p.Writes, PlanWrite{Path: docPath, Version: d.version, Body: d.body, Metadata: d.meta, Requires: d.requires, Fixes: d.fixes})
		}
	}
	rank := func(w *PlanWrite) int {
		switch {
		case w.Version == 0:
			return 0
		case len(w.Requires) > 0:
			return 1
		}
		return 2
	}
	slices.SortFunc(p.Writes, func(x, y PlanWrite) int {
		return cmp.Or(cmp.Compare(rank(&x), rank(&y)), cmp.Compare(x.Path, y.Path))
	})
	if len(p.Writes) > MaxPlanWrites {
		p.More = len(p.Writes) - MaxPlanWrites
		p.Writes = p.Writes[:MaxPlanWrites]
	}
	if len(p.Writes) > 0 {
		p.Hash = planHash(p.Writes)
	}
	return p
}

// planHash binds an apply to exactly these writes at exactly these versions.
func planHash(writes []PlanWrite) string {
	var parts []string
	for i := range writes {
		w := &writes[i]
		parts = append(parts, w.Path, strconv.Itoa(w.Version), w.Body, strings.Join(w.Requires, ","))
		for _, k := range slices.Sorted(maps.Keys(w.Metadata)) {
			parts = append(parts, k+"="+w.Metadata[k])
		}
	}
	return docwrite.Digest(parts...)
}
