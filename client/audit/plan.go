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

// PlanWrite is one document's composed fix; while the plan is built, the
// pending write, kept when it has a fix.
type PlanWrite struct {
	Path     string
	Version  int // the version read; 0 creates
	Body     string
	Metadata map[string]string
	Requires []string // writes that must land first: the hub a parent's bullets move to
	Fixes    []string // one line per change
}

func (w *PlanWrite) change(body, fix string) {
	w.Body = body
	w.Fixes = append(w.Fixes, fix)
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
	path, target string
	findings     []int // the broken link, and on a hub the stale entry
}

type adrLead struct {
	lead
	num, record string
}

type planner struct {
	a       *audit
	drafts  map[string]*PlanWrite
	unfixed []Finding
}

// plan composes the fixes the leads prove. Hubs are generated before
// orphans are placed, so an orphan sees the new hub and the room it frees.
func (a *audit) plan() *Plan {
	pl := &planner{a: a, drafts: map[string]*PlanWrite{}}
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
func (pl *planner) draft(p string) *PlanWrite {
	if w, ok := pl.drafts[p]; ok {
		return w
	}
	doc, ok := pl.a.docs[p]
	if !ok || !live(doc.status) {
		return nil
	}
	if v, err := docwrite.Version(doc.meta); err == nil && v > 0 {
		pl.drafts[p] = &PlanWrite{Path: p, Version: v, Body: doc.body, Metadata: docwrite.Carried(doc.meta)}
		return pl.drafts[p]
	}
	return nil
}

// open is draft for a document a lead names: one it cannot open is left to
// a rerun.
func (pl *planner) open(check, p string) *PlanWrite {
	w := pl.draft(p)
	if w == nil {
		pl.skip(check, p, p+" was not read with a version; rerun the audit")
	}
	return w
}

// current is p's body and metadata with the plan's changes so far.
func (pl *planner) current(p string) (body string, meta map[string]string) {
	if w, ok := pl.drafts[p]; ok {
		return w.Body, w.Metadata
	}
	return pl.a.docs[p].body, pl.a.docs[p].meta
}

// exists: a live document of the inventory, or a hub the plan creates.
func (pl *planner) exists(p string) bool {
	if w, ok := pl.drafts[p]; ok && w.Version == 0 {
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

// dirHub is the first of dir's hub.Paths the plan sees live, else the first.
func (pl *planner) dirHub(dir string) string {
	paths := hub.Paths(dir)
	if i := slices.IndexFunc(paths, pl.exists); i >= 0 {
		return paths[i]
	}
	return paths[0]
}

func (pl *planner) settle(findings ...int) {
	for _, i := range findings {
		pl.a.findings[i].Fixable = true
	}
}

func (pl *planner) skip(check, p, why string) {
	pl.unfixed = append(pl.unfixed, Finding{Check: check, Path: p, Detail: why})
}

// byPath groups leads by the document they name, paths sorted.
func byPath[T any](found []T, pathOf func(T) string) (paths []string, by map[string][]T) {
	by = map[string][]T{}
	for _, l := range found {
		by[pathOf(l)] = append(by[pathOf(l)], l)
	}
	return slices.Sorted(maps.Keys(by)), by
}

func (pl *planner) frontmatter() {
	for _, l := range pl.a.leads.frontmatter {
		w := pl.open(CheckFrontmatter, l.path)
		if w == nil {
			continue
		}
		kv, rest, ok := gate.Frontmatter(w.Body)
		if !ok {
			pl.skip(CheckFrontmatter, l.path, "the fence does not parse as key: value lines; strip it by hand")
			continue
		}
		keys := curate.Lift(kv, w.Metadata)
		if len(keys) == 0 {
			keys = []string{"none"}
		}
		w.change(rest, "frontmatter fence removed; metadata keys: "+strings.Join(keys, ", "))
		pl.settle(l.finding)
	}
}

func (pl *planner) journalType() {
	for _, l := range pl.a.leads.journalType {
		w := pl.open(CheckUntyped, l.path)
		if w == nil {
			continue
		}
		w.Metadata["type"] = "Journal"
		w.change(w.Body, "type Journal")
		pl.settle(l.finding)
	}
}

// archived drops a hub's bullets that link only archived documents, then
// unlinks the rest inline, keeping the text.
func (pl *planner) archived() {
	paths, by := byPath(pl.a.leads.archived, func(l archivedLead) string { return l.path })
	for _, doc := range paths {
		w := pl.open(CheckBrokenLinks, doc)
		if w == nil {
			continue
		}
		targets := map[string]bool{}
		for _, l := range by[doc] {
			targets[l.target] = true
		}
		body, bullets := w.Body, 0
		if gate.IsHub(doc, body) || pl.a.dirs[strings.TrimSuffix(doc, ".md")+"/"] {
			body, bullets = hub.WithoutLinks(body, doc, slices.Sorted(maps.Keys(targets)))
		}
		body, inline := unlinkInline(body, doc, targets)
		left := map[string]bool{}
		for _, dest := range links.Extract(body) {
			left[links.ResolvePath(doc, dest)] = true
		}
		for _, l := range by[doc] {
			if left[l.target] {
				pl.skip(CheckBrokenLinks, doc, "-> "+l.target+" is not an inline link; remove it by hand")
				continue
			}
			pl.settle(l.findings...)
		}
		if body != w.Body {
			w.change(body, fmt.Sprintf("links to archived documents removed: %d hub bullet(s), %d inline with the text kept", bullets, inline))
		}
	}
}

// unlinkInline replaces each inline link to a target with its text; n
// counts them. A reference-style link has no inline destination and stays.
func unlinkInline(body, docPath string, targets map[string]bool) (out string, n int) {
	var cuts []links.LinkInfo
	for _, l := range links.ExtractWithPositions(body) {
		if l.OpenBracket >= 0 && l.End > 0 && targets[links.ResolvePath(docPath, l.Dest)] {
			cuts = append(cuts, l)
		}
	}
	for i := len(cuts) - 1; i >= 0; i-- {
		c := &cuts[i]
		body = body[:c.OpenBracket] + body[c.OpenBracket+1:c.CloseBracket] + body[c.End:]
	}
	return body, len(cuts)
}

func (pl *planner) adrLinks() {
	paths, by := byPath(pl.a.leads.adr, func(l adrLead) string { return l.path })
	for _, doc := range paths {
		w := pl.open(CheckReferences, doc)
		if w == nil {
			continue
		}
		want := map[string]string{}
		for _, l := range by[doc] {
			want[l.num] = l.record
		}
		body, linked := linkADRs(w.Body, want)
		var nums []string
		for _, l := range by[doc] {
			if !linked[l.num] {
				pl.skip(CheckReferences, doc, "ADR "+l.num+" is mentioned only in links or headings; link it by hand")
				continue
			}
			nums = append(nums, l.num)
			pl.settle(l.finding)
		}
		if len(nums) > 0 {
			w.change(body, "linked ADR "+strings.Join(nums, ", "))
		}
	}
}

// linkADRs links the first mention of each wanted number (number to
// record) outside links and headings.
func linkADRs(body string, want map[string]string) (out string, linked map[string]bool) {
	type span struct{ start, end int }
	var skip []span
	for _, l := range links.ExtractWithPositions(body) {
		if l.OpenBracket >= 0 {
			skip = append(skip, span{l.OpenBracket, max(l.End, l.CloseBracket+1)})
		}
	}
	for _, h := range mdoutline.Headings(body) {
		end := len(body)
		if i := strings.IndexByte(body[h.Start:], '\n'); i >= 0 {
			end = h.Start + i
		}
		skip = append(skip, span{h.Start, end})
	}
	var adds []adrMention
	linked = map[string]bool{}
	for _, m := range adrMentions(body) {
		inside := slices.ContainsFunc(skip, func(s span) bool { return m.start >= s.start && m.start < s.end })
		if _, ok := want[m.num]; !ok || linked[m.num] || inside {
			continue
		}
		linked[m.num] = true
		adds = append(adds, m)
	}
	for i := len(adds) - 1; i >= 0; i-- {
		m := &adds[i]
		body = body[:m.start] + "[" + m.text + "](" + want[m.num] + ")" + body[m.end:]
	}
	return body, linked
}

// hubs generates each missing hub at D/index.md: the parent's bullets that
// link only documents in D/ move down, the parent keeps one bullet for D/,
// and every document the new hub is nearest to gets a bullet.
func (pl *planner) hubs() {
	for _, l := range pl.a.leads.hubs {
		dir := l.path
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
		linked := hub.Names(parent.Body, parentPath, hubPath)
		replace := dirBullet
		if linked {
			replace = ""
		}
		parentBody, moved := hub.MoveLinks(parent.Body, parentPath, inDir, replace)
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
		pl.drafts[hubPath] = &PlanWrite{
			Path:     hubPath,
			Body:     body,
			Metadata: map[string]string{"tags": strings.Join(protocol.MergeTags([]string{"index"}, []string{path.Base(dir)}), ","), "importance": generatedHubImportance},
			Fixes:    []string{fmt.Sprintf("hub created: %d bullet(s) moved from %s, %d added", len(moved), parentPath, added)},
		}
		parent.change(parentBody, fmt.Sprintf("%d bullet(s) moved to %s, one bullet for %s", len(moved), hubPath, dir))
		parent.Requires = append(parent.Requires, hubPath)
		pl.settle(l.finding)
	}
}

// orphans links each orphan from its nearest hub by the directory rule; a
// dated journal entry is reached by a link to its directory instead.
func (pl *planner) orphans() {
	for _, l := range pl.a.leads.orphans {
		switch p := l.path; {
		case pl.a.layout.JournalEntry(p):
			journalDir := path.Dir(p) + "/"
			scope := strings.TrimSuffix(journalDir, pl.a.layout.Journal)
			hubPath := pl.dirHub(scope)
			section, bullet := hub.JournalBullet(hubPath, scope, pl.a.layout.Journal)
			pl.link(l, hubPath, &hub.Link{Target: journalDir, Section: section, Bullet: bullet})
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
	w := pl.draft(hubPath)
	if w == nil {
		pl.skip(CheckOrphans, l.path, "its hub "+hubPath+" is not in scope")
		return
	}
	switch {
	case hub.Names(w.Body, hubPath, l.path):
	case gate.OutboundDocs(w.Body) >= gate.HubLinkMax:
		pl.skip(CheckOrphans, l.path, fmt.Sprintf("its hub %s is at the cap of %d links", hubPath, gate.HubLinkMax))
		return
	default:
		w.change(hub.WithBullet(w.Body, want.Section, want.Bullet), "bullet for "+want.Target)
	}
	pl.settle(l.finding)
}

// finish orders the writes (created hubs, then the parents that need them,
// then by path), cuts the batch at MaxPlanWrites and hashes it.
func (pl *planner) finish() *Plan {
	p := &Plan{Unfixed: pl.unfixed}
	for _, w := range pl.drafts {
		if len(w.Fixes) > 0 {
			p.Writes = append(p.Writes, *w)
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
		clear(p.Writes[MaxPlanWrites:]) // the cut writes' bodies must not outlive the plan
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
