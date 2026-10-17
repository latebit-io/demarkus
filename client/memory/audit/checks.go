package audit

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/client/memory/promote"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/mdoutline"
	"github.com/latebit-io/demarkus/protocol/stylerules"
)

// Check names in report order.
const (
	CheckBrokenLinks   = "Broken links"
	CheckUnresolved    = "Unresolved links"
	CheckOrphans       = "Orphans"
	CheckStaleIndex    = "Stale index entries"
	CheckMissingHub    = "Missing hub"
	CheckUntitled      = "Untitled"
	CheckADRSequence   = "ADR sequence"
	CheckUntagged      = "Untagged"
	CheckUntyped       = "Untyped"
	CheckPolicy        = "Policy compliance"
	CheckPolicyFloor   = "Policy floor"
	CheckDuplicates    = "Duplicate content"
	CheckHubShape      = "Hub shape"
	CheckOversized     = "Oversized"
	CheckDocumentShape = "Document shape"
	CheckStyle         = "Style"
	CheckFrontmatter   = "In-body frontmatter"
	CheckContent       = "Secrets and personal data"
	CheckStyleGuide    = "Style guide stale"
	CheckReferences    = "Dangling and unlinked references"
	CheckLostMetadata  = "Metadata lost across versions"
	CheckPromotion     = "Promotion candidates"
	CheckTokenDrift    = "Token drift"
)

// CheckOrder is the report section order, most actionable first.
var CheckOrder = []string{
	CheckBrokenLinks, CheckUnresolved, CheckOrphans, CheckStaleIndex, CheckMissingHub, CheckUntitled,
	CheckADRSequence, CheckUntagged, CheckUntyped, CheckPolicy, CheckPolicyFloor, CheckDuplicates, CheckHubShape, CheckOversized,
	CheckDocumentShape, CheckStyle, CheckFrontmatter, CheckContent, CheckStyleGuide, CheckReferences, CheckLostMetadata, CheckPromotion, CheckTokenDrift,
}

// checkLinks: a target absent from the live inventory is broken when a fetch
// says not-found or archived, fine when it serves (outside the scope), any
// other status is unresolved, and targets past the budget go to Coverage.
func (a *audit) checkLinks(ctx context.Context) {
	confirmed := map[string]string{}
	var unconfirmed []string
	for _, p := range a.order {
		for _, target := range a.docs[p].links {
			if _, ok := a.docs[target]; ok {
				continue
			}
			status, known := confirmed[target]
			if !known {
				if len(confirmed) >= MaxLinkConfirms {
					unconfirmed = append(unconfirmed, p+" -> "+target)
					continue
				}
				status = a.confirm(ctx, target)
				confirmed[target] = status
			}
			switch {
			case live(status):
			case status == protocol.StatusArchived:
				a.leads.archived = append(a.leads.archived, archivedLead{path: p, target: target, findings: a.brokenLink(p, target, status, "drop the link")})
			case status == protocol.StatusNotFound:
				a.brokenLink(p, target, status, "fix the link or restore the target")
			default:
				a.add(CheckUnresolved, p, "-> "+target+" (fetch: "+status+")", "check access or the server, then re-run")
			}
		}
	}
	if len(unconfirmed) > 0 {
		shown := unconfirmed[:min(len(unconfirmed), maxUnconfirmedShown)]
		a.note("link confirmation budget of %d fetches spent; %d links unconfirmed, first %d: %s", MaxLinkConfirms, len(unconfirmed), len(shown), strings.Join(shown, ", "))
	}
}

func (a *audit) confirm(ctx context.Context, target string) string {
	resp, err := a.store.Fetch(ctx, target)
	if err != nil {
		a.note("confirm %s failed: %v", target, err)
		return "error"
	}
	return resp.Status
}

// brokenLink: a hub's broken link is also a stale index entry; the
// findings' indices are returned.
func (a *audit) brokenLink(p, target, status, fix string) []int {
	found := []int{a.add(CheckBrokenLinks, p, "-> "+target+" (fetch: "+status+")", fix)}
	if path.Base(p) == "index.md" {
		word := "missing"
		if status == protocol.StatusArchived {
			word = "archived"
		}
		found = append(found, a.add(CheckStaleIndex, p, "-> "+target+" (hub links a "+word+" document)", fix))
	}
	return found
}

// checkOrphans: a live document nothing in scope links to, the hub
// excepted. A link to a directory reaches every document directly in it, so
// a hub that links /journal/ covers each dated entry.
func (a *audit) checkOrphans() {
	inbound, reachedDirs := map[string]bool{}, map[string]bool{}
	for _, p := range a.order {
		for _, t := range a.docs[p].links {
			inbound[t] = true
		}
		for _, dir := range a.docs[p].dirLinks {
			reachedDirs[dir] = true
		}
	}
	scopeHub := hub.Paths(a.opts.Scope)[0]
	for _, p := range a.order {
		d := a.docs[p]
		if p == scopeHub || !live(d.status) || inbound[p] || reachedDirs[path.Dir(p)+"/"] {
			continue
		}
		a.leads.orphans = append(a.leads.orphans, lead{p, a.add(CheckOrphans, p, "no inbound link from any document in scope", "link it from its hub, or /soul-archive it")})
	}
}

// checkHubs: a project (each top-level directory at the root, else the
// scope) holding documents a hub should name and no hub at hub.Paths.
func (a *audit) checkHubs(ctx context.Context) {
	counts := map[string]int{}
	for _, p := range a.order {
		if !hub.Linkable(p) {
			continue
		}
		project := a.opts.Scope
		if project == "/" {
			head, _, nested := strings.Cut(strings.TrimPrefix(p, "/"), "/")
			if !nested {
				continue
			}
			project = "/" + head + "/"
		}
		counts[project]++
	}
	for _, dir := range slices.Sorted(maps.Keys(counts)) {
		switch {
		case a.hasHub(ctx, dir):
		case a.layout.JournalDir(dir):
			a.add(CheckMissingHub, dir, fmt.Sprintf("%d documents that are not dated entries; a journal needs only its directory link", counts[dir]), "rename them to <date>.md or move them out")
		default:
			a.leads.hubs = append(a.leads.hubs, lead{dir, a.add(CheckMissingHub, dir, fmt.Sprintf("%d documents and no %s", counts[dir], strings.Join(hub.Paths(dir), " or ")), "publish a hub linking each document once")})
		}
	}
}

// hasHub: the inventory holds a hub of dir, or one outside the scope (the
// scope's own <dir>.md) answers a fetch; a failed fetch is a coverage gap.
func (a *audit) hasHub(ctx context.Context, dir string) bool {
	for _, p := range hub.Paths(dir) {
		if _, ok := a.docs[p]; ok {
			return true
		}
		if strings.HasPrefix(p, a.opts.Scope) {
			continue
		}
		switch status := a.confirm(ctx, p); status {
		case protocol.StatusNotFound, protocol.StatusArchived:
			continue
		case protocol.StatusOK:
		default:
			a.note("hub check of %s skipped: %s returned %s", dir, p, status)
		}
		return true
	}
	return false
}

func (a *audit) checkTitles() {
	for _, p := range a.order {
		if d := a.docs[p]; live(d.status) && title(d) == "" {
			a.add(CheckUntitled, p, "no H1 and no title metadata", "add a # H1")
		}
	}
}

// adrIndex maps each adr directory to its numbered records.
func (a *audit) adrIndex() map[string]map[int][]string {
	byDir := map[string]map[int][]string{}
	for _, p := range a.order {
		dir := path.Dir(p)
		m := adrFileRe.FindStringSubmatch(path.Base(p))
		if path.Base(dir) != "adr" || m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1]) // the regex guarantees four digits
		if byDir[dir] == nil {
			byDir[dir] = map[int][]string{}
		}
		byDir[dir][n] = append(byDir[dir][n], p)
	}
	return byDir
}

// checkADRSequence: per adr directory, duplicate or gapped NNNN prefixes.
func (a *audit) checkADRSequence() {
	byDir := a.adrIndex()
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		nums := byDir[dir]
		highest := 0
		for n, paths := range nums {
			highest = max(highest, n)
			if len(paths) > 1 {
				slices.Sort(paths)
				a.add(CheckADRSequence, dir+"/", fmt.Sprintf("%0*d used by %s", adrNumberWidth, n, strings.Join(paths, ", ")), "renumber one of them")
			}
		}
		var gaps []string
		for n := 1; n <= highest; n++ {
			if _, ok := nums[n]; !ok {
				gaps = append(gaps, fmt.Sprintf("%0*d", adrNumberWidth, n))
			}
		}
		if len(gaps) > 0 {
			a.add(CheckADRSequence, dir+"/", "missing "+strings.Join(gaps, ", "), "restore the record or note the retired number in the hub")
		}
	}
}

// checkMetadata: tags and OKF type, navigation leaves exempt from type.
func (a *audit) checkMetadata() {
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		if strings.TrimSpace(d.meta["tags"]) == "" {
			a.add(CheckUntagged, p, "no tags; findable only by title or path", "re-publish with metadata.tags")
		}
		if gate.NavExempt(p) {
			continue
		}
		if t := d.meta["type"]; t == "" || t == "Document" {
			detail := "type missing"
			if t != "" {
				detail = "type=Document (generic default)"
			}
			i := a.add(CheckUntyped, p, detail, "re-publish with metadata.type (Reference, Decision, Architecture, Plan, Journal, Guide)")
			if a.layout.JournalEntry(p) {
				a.leads.journalType = append(a.leads.journalType, lead{p, i})
			}
		}
	}
}

func (a *audit) checkDuplicates() {
	byHash := map[string][]string{}
	for _, p := range a.order {
		if d := a.docs[p]; live(d.status) && d.meta["content-hash"] != "" {
			byHash[d.meta["content-hash"]] = append(byHash[d.meta["content-hash"]], p)
		}
	}
	for _, h := range slices.Sorted(maps.Keys(byHash)) {
		paths := byHash[h]
		if len(paths) < 2 {
			continue
		}
		slices.Sort(paths)
		a.add(CheckDuplicates, paths[0], "identical body at "+strings.Join(paths[1:], ", "), "keep one and link the rest")
	}
}

// checkShape runs the gate once per body and files each finding under the
// check its rule id belongs to. A missing H1 is the Untitled check's.
func (a *audit) checkShape() {
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		for _, problem := range gate.AuditProblems(p, d.body, d.meta["description"]) {
			id, _, _ := strings.Cut(problem, ": ")
			switch id {
			case stylerules.H1:
			case stylerules.Frontmatter:
				a.leads.frontmatter = append(a.leads.frontmatter, lead{p, a.add(CheckFrontmatter, p, problem, "strip the block and re-publish with metadata out of band")})
			case stylerules.EmDash, stylerules.DuplicateHeading:
				a.add(CheckStyle, p, problem, "/soul-curate "+p)
			case stylerules.HubSize, stylerules.HubBullet, stylerules.HubFanout:
				a.add(CheckHubShape, p, problem, "/soul-curate "+p)
			case stylerules.Size:
				a.add(CheckOversized, p, problem, "/soul-curate "+p+" (hub plus topic files)")
			case stylerules.DescriptionShort, stylerules.DescriptionRestatesTitle, stylerules.DescriptionPlaceholder, stylerules.DescriptionMismatch:
				a.add(CheckDocumentShape, p, problem, "re-publish with metadata.description equal to the H1 summary line")
			default:
				a.add(CheckDocumentShape, p, problem, "/soul-curate "+p)
			}
		}
	}
}

// checkReferences: ADR mentions in prose that are not links. Resolved against
// the inventory and the citing body's own links, fenced code excluded.
func (a *audit) checkReferences() {
	byDir := a.adrIndex()
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		own := ""
		if m := adrFileRe.FindStringSubmatch(path.Base(p)); m != nil && path.Base(path.Dir(p)) == "adr" {
			own = m[1]
		}
		seen := map[string]bool{}
		for _, m := range adrMentions(d.body) {
			if m.num == own || seen[m.num] {
				continue
			}
			seen[m.num] = true
			ref := resolveADR(byDir, p, m.n)
			switch {
			case len(ref.records) == 0:
				a.add(CheckReferences, p, fmt.Sprintf("%q dangling: no adr/%s-*.md in scope", m.text, m.num), "restore the record or drop the reference")
			case slices.ContainsFunc(ref.records, func(r string) bool { return slices.Contains(d.links, r) }):
			case ref.foreign:
				a.add(CheckReferences, p, fmt.Sprintf("%q not in %s: %s is another series", m.text, ref.series, ref.records[0]), "link it if that record is meant")
			case len(ref.records) > 1:
				a.add(CheckReferences, p, fmt.Sprintf("%q ambiguous: %s share the number", m.text, strings.Join(ref.records, " and ")), "renumber one (ADR sequence), then link the record meant")
			default:
				i := a.add(CheckReferences, p, fmt.Sprintf("%q unlinked: %s exists but the body has no link to it", m.text, ref.records[0]), "[ADR "+m.num+"]("+ref.records[0]+")")
				a.leads.adr = append(a.leads.adr, adrLead{lead: lead{p, i}, num: m.num, record: ref.records[0]})
			}
		}
	}
}

// adrMention is one ADR citation outside code: its text, number (padded and
// as read) and offsets. A range ("ADR 0001 to 0027") cites neither end.
type adrMention struct {
	text, num  string
	n          int
	start, end int
}

// adrMentions are the body's ADR citations in order; the check and the fix
// read the same ones.
func adrMentions(body string) []adrMention {
	masked := stripCode(body)
	var out []adrMention
	for _, m := range adrReferenceRe.FindAllStringSubmatchIndex(masked, -1) {
		if m[4] >= 0 {
			continue
		}
		n, err := strconv.Atoi(masked[m[2]:m[3]])
		if err != nil {
			continue // unreachable: the regex matches digits only
		}
		out = append(out, adrMention{text: body[m[0]:m[1]], num: fmt.Sprintf("%0*d", adrNumberWidth, n), n: n, start: m[0], end: m[1]})
	}
	return out
}

// adrRef is what one ADR number names from a citing document.
type adrRef struct {
	records []string // sorted; more than one is a duplicated number
	series  string   // the nearest series walking up, with its slash; "" when none
	foreign bool     // records are in a series above it: another project's numbering
}

// resolveADR finds the number in the nearest adr directory walking up that
// has it. Only a record in the document's own (nearest) series is provable.
func resolveADR(byDir map[string]map[int][]string, from string, n int) adrRef {
	var ref adrRef
	for dir := path.Dir(from); ; dir = path.Dir(dir) {
		series := path.Join(dir, "adr")
		if nums, ok := byDir[series]; ok {
			if ref.series == "" {
				ref.series = series + "/"
			}
			if records := nums[n]; len(records) > 0 {
				ref.records = slices.Sorted(slices.Values(records))
				ref.foreign = ref.series != series+"/"
				return ref
			}
		}
		if dir == "/" || dir == "." {
			return ref
		}
	}
}

// checkLostMetadata (deep): an untagged document with history whose earlier
// version carried tags, reported as a repair candidate.
func (a *audit) checkLostMetadata(ctx context.Context) {
	fetches := 0
	spend := func(where string) bool {
		if fetches >= MaxVersionFetches {
			a.note("version budget of %d fetches spent at %s; later history unchecked", MaxVersionFetches, where)
			return false
		}
		fetches++
		return true
	}
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) || strings.TrimSpace(d.meta["tags"]) != "" {
			continue
		}
		// A missing or unparsable version leaves nothing to walk; the
		// document is still reported as untagged by checkMetadata.
		current, _ := strconv.Atoi(d.meta["version"])
		if current < 2 || !spend(p) {
			continue
		}
		vr, err := a.store.Versions(ctx, p)
		if err != nil || vr.Status != protocol.StatusOK {
			a.note("versions %s: %s", p, statusOrErr(vr.Status, err))
			continue
		}
		if vr.Metadata["chain-valid"] == "false" {
			a.add(CheckLostMetadata, p, "inconclusive: "+vr.Metadata["chain-error"], "")
			continue
		}
		found := false
		for v := current - 1; v >= 1 && current-v <= MaxVersionsPerDoc && spend(p); v-- {
			resp, err := a.store.Fetch(ctx, protocol.VersionPath(p, v))
			if err != nil || resp.Status != protocol.StatusOK {
				a.note("fetch %s: %s", protocol.VersionPath(p, v), statusOrErr(resp.Status, err))
				break
			}
			if strings.TrimSpace(resp.Metadata["tags"]) == "" {
				continue
			}
			found = true
			a.add(CheckLostMetadata, p, fmt.Sprintf("candidate: tagged at v%d (%s), none at v%d", v, describeMeta(resp.Metadata), current),
				fmt.Sprintf("force-fetch v%d, publish that body with its metadata map plus the v%d fields, expected_version %d, on_conflict fail; confirm with the user first", current, v, current))
			break
		}
		if !found {
			a.add(CheckLostMetadata, p, fmt.Sprintf("untagged through the last %d versions; never tagged", min(current-1, MaxVersionsPerDoc)), "curation, not repair")
		}
	}
}

// checkPromotion (on request): live documents an adr directory, a shared
// type or high importance marks as team knowledge; hubs and staging
// directories never qualify. The promoted ones (tag or marker) are counted.
func (a *audit) checkPromotion() {
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) || gate.NavExempt(p) {
			continue
		}
		why := a.layout.Qualifies(p, d.meta)
		if why == "" {
			continue
		}
		if promote.IsPromoted(d.meta, d.body) {
			a.promoted++
			continue
		}
		a.add(CheckPromotion, p, why, "/promote "+p)
	}
}

func describeMeta(m map[string]string) string {
	parts := []string{strconv.Itoa(len(protocol.SplitTags(m["tags"]))) + " tags"}
	for _, k := range []string{"importance", "type"} {
		if v := m[k]; v != "" {
			parts = append(parts, k+" "+v)
		}
	}
	return strings.Join(parts, ", ")
}

func statusOrErr(status string, err error) string {
	if err != nil {
		return err.Error()
	}
	return status
}

// stripCode blanks code (fences, indented blocks, spans) so ADR mentions in
// examples do not count.
func stripCode(body string) string {
	out := []byte(body)
	for _, seg := range mdoutline.CodeSegments(body) {
		for i := seg.Start; i < seg.Stop; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return string(out)
}

func sortedUnique(in []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(in)))
}
