package doctor

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/gate"
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
	CheckDuplicates    = "Duplicate content"
	CheckHubShape      = "Hub shape"
	CheckOversized     = "Oversized"
	CheckDocumentShape = "Document shape"
	CheckStyle         = "Style"
	CheckFrontmatter   = "In-body frontmatter"
	CheckReferences    = "Dangling and unlinked references"
	CheckLostMetadata  = "Metadata lost across versions"
	CheckTokenDrift    = "Token drift"
)

// CheckOrder is the report section order, most actionable first.
var CheckOrder = []string{
	CheckBrokenLinks, CheckUnresolved, CheckOrphans, CheckStaleIndex, CheckMissingHub, CheckUntitled,
	CheckADRSequence, CheckUntagged, CheckUntyped, CheckDuplicates, CheckHubShape, CheckOversized,
	CheckDocumentShape, CheckStyle, CheckFrontmatter, CheckReferences, CheckLostMetadata, CheckTokenDrift,
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
				a.brokenLink(p, target, status, "drop the link")
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

// brokenLink: a hub's broken link is also a stale index entry.
func (a *audit) brokenLink(p, target, status, fix string) {
	a.add(CheckBrokenLinks, p, "-> "+target+" (fetch: "+status+")", fix)
	if path.Base(p) == "index.md" {
		word := "missing"
		if status == protocol.StatusArchived {
			word = "archived"
		}
		a.add(CheckStaleIndex, p, "-> "+target+" (hub links a "+word+" document)", fix)
	}
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
	hub := a.hub()
	for _, p := range a.order {
		d := a.docs[p]
		if p == hub || !live(d.status) || inbound[p] || reachedDirs[path.Dir(p)+"/"] {
			continue
		}
		a.add(CheckOrphans, p, "no inbound link from any document in scope", "link it from its hub, or /soul-archive it")
	}
}

// checkHubs: a project subtree with documents and no index.md. At the root
// every top-level directory is a project; under a project scope, the scope.
func (a *audit) checkHubs() {
	counts := map[string]int{}
	for _, p := range a.order {
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
		if _, ok := a.docs[path.Join(dir, "index.md")]; !ok {
			a.add(CheckMissingHub, dir, fmt.Sprintf("%d documents and no index.md", counts[dir]), "publish a hub linking each document once")
		}
	}
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
		if gate.NavExempt(path.Base(p)) {
			continue
		}
		if t := d.meta["type"]; t == "" || t == "Document" {
			detail := "type missing"
			if t != "" {
				detail = "type=Document (generic default)"
			}
			a.add(CheckUntyped, p, detail, "re-publish with metadata.type (Reference, Decision, Architecture, Plan, Journal, Guide)")
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

// checkShape runs the gate's hub, document-shape and style rules over each body.
func (a *audit) checkShape() {
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		hub := gate.HubProblems(p, d.body)
		for _, problem := range hub {
			a.add(CheckHubShape, p, problem, "/soul-curate "+p)
		}
		if len(d.body) >= oversizedBytes && len(hub) == 0 {
			a.add(CheckOversized, p, fmt.Sprintf("%d KB; a plain fetch returns an outline", len(d.body)/1024), "/soul-curate "+p+" (hub plus topic files)")
		}
		for _, problem := range gate.ShapeProblems(p, d.body, d.headings) {
			a.add(CheckDocumentShape, p, problem, "/soul-curate "+p)
		}
		if n := gate.EmDashCount(d.body); n > 0 {
			a.add(CheckStyle, p, fmt.Sprintf("%d em dash(es)", n), "replace with comma, colon, semicolon or parentheses")
		}
		if dups := gate.DuplicateHeadings(d.headings); len(dups) > 0 {
			a.add(CheckStyle, p, "duplicate headings "+strings.Join(dups, ", ")+" (a -1 suffix shifts inbound anchors)", "make headings unique")
		}
		if gate.OpensWithFrontmatter(d.body) {
			a.add(CheckFrontmatter, p, "body opens with a --- block; the server stores it literally", "strip the block and re-publish with metadata out of band")
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
		mentions := adrReferenceRe.FindAllStringSubmatch(stripFences(d.body), -1)
		if len(mentions) == 0 {
			continue
		}
		own := ""
		if m := adrFileRe.FindStringSubmatch(path.Base(p)); m != nil && path.Base(path.Dir(p)) == "adr" {
			own = m[1]
		}
		seen := map[string]bool{}
		for _, m := range mentions {
			n, _ := strconv.Atoi(m[1])
			num := fmt.Sprintf("%0*d", adrNumberWidth, n)
			if num == own || seen[num] {
				continue
			}
			seen[num] = true
			target := adrTarget(byDir, p, n)
			switch {
			case target == "":
				a.add(CheckReferences, p, fmt.Sprintf("%q dangling: no adr/%s-*.md in scope", m[0], num), "restore the record or drop the reference")
			case !slices.Contains(d.links, target):
				a.add(CheckReferences, p, fmt.Sprintf("%q unlinked: %s exists but the body has no link to it", m[0], target), "[ADR "+num+"]("+target+")")
			}
		}
	}
}

// adrTarget finds adr/NNNN-*.md in the citing document's own project: the
// nearest adr directory walking up from the document. A number that exists
// only in another project's series is not this document's reference.
func adrTarget(byDir map[string]map[int][]string, from string, n int) string {
	for dir := path.Dir(from); ; dir = path.Dir(dir) {
		if paths := byDir[path.Join(dir, "adr")][n]; len(paths) > 0 {
			return slices.Min(paths)
		}
		if dir == "/" || dir == "." {
			return ""
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

// stripFences blanks fenced code blocks so ADR mentions in examples do not count.
func stripFences(body string) string {
	var out strings.Builder
	out.Grow(len(body))
	fence := ""
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence == "" && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")):
			fence = trimmed[:3]
		case fence != "" && strings.HasPrefix(trimmed, fence):
			fence = ""
		case fence == "":
			out.WriteString(line)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func sortedUnique(in []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(in)))
}
