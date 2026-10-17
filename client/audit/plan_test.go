package audit

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func hubDoc(body string) fakeDoc {
	return fakeDoc{Body: body, Meta: map[string]string{"tags": "index"}}
}

// planOf audits the store and returns the report with its writes by path.
func planOf(t *testing.T, docs map[string]fakeDoc) (r *Report, writes map[string]PlanWrite) {
	t.Helper()
	r, err := Run(context.Background(), newFake(docs), Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	writes = map[string]PlanWrite{}
	for _, w := range r.Plan.Writes {
		writes[w.Path] = w
	}
	return r, writes
}

// fixable is every finding of the check the plan settles, as findings() prints it.
func fixable(r *Report, check string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Check == check && f.Fixable {
			out = append(out, fmt.Sprintf("%s: %s", f.Path, f.Detail))
		}
	}
	return out
}

func unfixed(r *Report) []string {
	out := make([]string, 0, len(r.Plan.Unfixed))
	for _, f := range r.Plan.Unfixed {
		out = append(out, f.Path+": "+f.Detail)
	}
	return out
}

// A hub drops a bullet that links only archived documents; elsewhere the
// link goes and its text stays.
func TestPlanDropsLinksToArchivedDocuments(t *testing.T) {
	r, writes := planOf(t, map[string]fakeDoc{
		"/index.md": hubDoc("# Hub\n\nThe store.\n\n- [Old](/old.md): gone\n- [Notes](/notes.md): live\n- [Both](/old.md) and [notes](/notes.md)\n"),
		"/notes.md": tagged("# Notes\n\nSee [the *old* plan](/old.md \"title (v1)\") and [itself](/notes.md).\n"),
		"/old.md":   {Body: "# Old\n", Status: protocol.StatusArchived},
	})
	if got := writes["/index.md"].Body; got != "# Hub\n\nThe store.\n\n- [Notes](/notes.md): live\n- Both and [notes](/notes.md)\n" {
		t.Errorf("hub = %q", got)
	}
	if got := writes["/notes.md"].Body; got != "# Notes\n\nSee the *old* plan and [itself](/notes.md).\n" {
		t.Errorf("prose = %q", got)
	}
	if len(fixable(r, CheckBrokenLinks)) != 2 || len(fixable(r, CheckStaleIndex)) != 1 {
		t.Errorf("fixable broken %v, stale %v", fixable(r, CheckBrokenLinks), fixable(r, CheckStaleIndex))
	}
}

// The first mention outside code, links and headings becomes a link; a
// number mentioned nowhere else is left reported.
func TestPlanLinksUnlinkedADRReferences(t *testing.T) {
	r, writes := planOf(t, map[string]fakeDoc{
		"/index.md":        hubDoc("# Hub\n\nThe store.\n\n- [ADRs](/adr/index.md)\n- [Notes](/notes.md)\n- [Heads](/heads.md)\n"),
		"/adr/index.md":    hubDoc("# ADRs\n\nRecords.\n\n- [One](/adr/0001-one.md)\n- [Two](/adr/0002-two.md)\n"),
		"/adr/0001-one.md": tagged("# 0001. One\n\nSummary.\n"),
		"/adr/0002-two.md": tagged("# 0002. Two\n\nSummary.\n"),
		"/notes.md":        tagged("# Notes\n\n## Why ADR 0001\n\n`ADR 0001` and [ADR 0001 notes](/heads.md), then ADR 0001 and ADR-0002, again ADR 0001.\n"),
		"/heads.md":        tagged("# Heads\n\n## ADR 0002\n\nSummary.\n"),
	})
	if got := writes["/notes.md"].Body; got != "# Notes\n\n## Why ADR 0001\n\n`ADR 0001` and [ADR 0001 notes](/heads.md), then [ADR 0001](/adr/0001-one.md) and [ADR-0002](/adr/0002-two.md), again ADR 0001.\n" {
		t.Errorf("notes = %q", got)
	}
	if _, ok := writes["/heads.md"]; ok || !slices.Contains(unfixed(r), "/heads.md: ADR 0002 is mentioned only in links or headings; link it by hand") {
		t.Errorf("heading only: writes %v, unfixed %v", writes["/heads.md"], unfixed(r))
	}
	if got := fixable(r, CheckReferences); len(got) != 2 {
		t.Errorf("fixable references = %v", got)
	}
}

// An orphan gets a bullet in its nearest hub by the directory rule; a dated
// journal entry is reached by a link to its directory, even when the hub
// links other entries one by one; a hub at the cap is left reported.
func TestPlanLinksOrphansFromTheNearestHub(t *testing.T) {
	docs := map[string]fakeDoc{
		"/index.md":                   hubDoc("# Hub\n\nThe store.\n\n- [P](/proj/index.md)\n- [Full](/full/index.md)\n"),
		"/proj/index.md":              hubDoc("# P\n\nThe project.\n\n- [A](/proj/a.md)\n- [Day one](journal/2026-01-01.md)\n"),
		"/proj/a.md":                  tagged("# A\n\nSummary.\n"),
		"/proj/orphan.md":             tagged("# Orphan\n\nWhat it holds.\n"),
		"/proj/journal/2026-01-01.md": tagged("# Day one\n\nNotes.\n"),
		"/proj/journal/2026-01-02.md": tagged("# Day two\n\nNotes.\n"),
		"/full/x.md":                  tagged("# X\n\nSummary.\n"),
	}
	var full strings.Builder
	full.WriteString("# Full\n\nAt the cap.\n\n")
	for i := range 40 {
		p := fmt.Sprintf("/full/d%02d.md", i)
		docs[p] = tagged("# D\n\nSummary.\n")
		fmt.Fprintf(&full, "- [D](%s)\n", p)
	}
	docs["/full/index.md"] = hubDoc(full.String())
	r, writes := planOf(t, docs)
	want := "# P\n\nThe project.\n\n- [A](/proj/a.md)\n- [Day one](journal/2026-01-01.md)\n- [Orphan](/proj/orphan.md): What it holds.\n\n## Journal\n\n- [Journal](journal/): dated session notes\n"
	if got := writes["/proj/index.md"].Body; got != want {
		t.Errorf("project hub = %q", got)
	}
	if !slices.Contains(unfixed(r), "/full/x.md: its hub /full/index.md is at the cap of 40 links") {
		t.Errorf("unfixed = %v", unfixed(r))
	}
	if got := fixable(r, CheckOrphans); len(got) != 2 {
		t.Errorf("fixable orphans = %v", got)
	}
}

// A project whose hub is its <dir>.md links the journal from there, the link
// relative to that hub; no index.md is created beside it.
func TestPlanLinksAJournalFromADirNamedHub(t *testing.T) {
	r, writes := planOf(t, map[string]fakeDoc{
		"/index.md":                    hubDoc("# Hub\n\nThe store.\n\n- [Notes](/notes.md)\n"),
		"/notes.md":                    hubDoc("# Notes\n\nThe topics.\n\n- [A](/notes/a.md)\n"),
		"/notes/a.md":                  tagged("# A\n\nSummary.\n"),
		"/notes/journal/2026-01-01.md": tagged("# Day one\n\nNotes.\n"),
	})
	want := "# Notes\n\nThe topics.\n\n- [A](/notes/a.md)\n\n## Journal\n\n- [Journal](notes/journal/): dated session notes\n"
	if got := writes["/notes.md"].Body; got != want {
		t.Errorf("dir-named hub = %q", got)
	}
	if _, ok := writes["/notes/index.md"]; ok || len(fixable(r, CheckOrphans)) != 1 {
		t.Errorf("writes = %v, fixable orphans = %v", slices.Collect(maps.Keys(writes)), fixable(r, CheckOrphans))
	}
}

// A missing hub is generated at D/index.md: the parent's bullets that link
// only into D/ move down (relative links made absolute), the parent keeps
// one bullet for D/ where the first was, and the rest of D/ is listed.
func TestPlanGeneratesAMissingHub(t *testing.T) {
	r, writes := planOf(t, map[string]fakeDoc{
		"/index.md":          hubDoc("# Hub\n\nThe store.\n\n- [One](/research/one.md): first\n- [Notes](/notes.md)\n  - [Two](research/two.md): relative\n- [Mixed](/research/one.md) and [n](/notes.md)\n"),
		"/notes.md":          tagged("# Notes\n\nSummary.\n"),
		"/research/one.md":   tagged("# One\n\nSummary.\n"),
		"/research/two.md":   tagged("# Two\n\nSummary.\n"),
		"/research/three.md": tagged("# Three\n\nWhat it found.\n"),
	})
	if len(r.Plan.Writes) != 2 || r.Plan.Writes[0].Path != "/research/index.md" || r.Plan.Writes[0].Version != 0 {
		t.Fatalf("writes = %+v", r.Plan.Writes)
	}
	created := r.Plan.Writes[0]
	if created.Body != "# Research\n\nResearch documents, one link each.\n\n- [One](/research/one.md): first\n- [Two](/research/two.md): relative\n- [Three](/research/three.md): What it found.\n" {
		t.Errorf("created = %q", created.Body)
	}
	if created.Metadata["tags"] != "index,research" {
		t.Errorf("created metadata = %v", created.Metadata)
	}
	parent := writes["/index.md"]
	if parent.Body != "# Hub\n\nThe store.\n\n- [Research](/research/): Research documents, one link each.\n- [Notes](/notes.md)\n- [Mixed](/research/one.md) and [n](/notes.md)\n" || !slices.Equal(parent.Requires, []string{"/research/index.md"}) {
		t.Errorf("parent = %+v", parent)
	}
	if len(fixable(r, CheckMissingHub)) != 1 || len(fixable(r, CheckOrphans)) != 1 {
		t.Errorf("fixable hub %v, orphans %v", fixable(r, CheckMissingHub), fixable(r, CheckOrphans))
	}
	// Nothing else shows a created document, so the proposal carries it whole.
	if md := r.Plan.Markdown("/"); !strings.Contains(md, "- /research/index.md (create): hub created: 2 bullet(s) moved from /index.md, 1 added\n\n~~~~markdown\n"+created.Body+"~~~~\n") ||
		!strings.Contains(md, "- /index.md (v1, after /research/index.md): ") {
		t.Errorf("proposal:\n%s", md)
	}
}

// Over the cap a missing hub is left to judgment; a journal directory needs
// no hub, so its finding names the documents that are not dated entries.
func TestPlanReportsMissingHubsItCannotGenerate(t *testing.T) {
	docs := map[string]fakeDoc{
		"/index.md":         hubDoc("# Hub\n\nThe store.\n\n- [Big](/big/)\n- [Journal](/journal/)\n"),
		"/journal/notes.md": tagged("# Notes\n\nNot a day.\n"),
	}
	for i := range 41 {
		docs[fmt.Sprintf("/big/d%02d.md", i)] = tagged("# D\n\nSummary.\n")
	}
	r, writes := planOf(t, docs)
	if len(writes) != 0 {
		t.Errorf("writes = %v", writes)
	}
	if got := unfixed(r); !slices.Equal(got, []string{"/big/: over the cap: one hub would link 41 documents, 40 is the cap; publish /big.md as its hub or add second-level hubs"}) {
		t.Errorf("unfixed = %v", got)
	}
	if got := findings(r, CheckMissingHub); !slices.Contains(got, "/journal/: 1 documents that are not dated entries; a journal needs only its directory link") {
		t.Errorf("missing hub findings = %v", got)
	}
}

// Fixes on one document compose into one write at the version read: the
// fence lifted into metadata, a dated journal entry typed Journal.
func TestPlanComposesFrontmatterAndJournalType(t *testing.T) {
	_, writes := planOf(t, map[string]fakeDoc{
		"/index.md":              hubDoc("# Hub\n\nThe store.\n\n- [Fm](/fm.md)\n- [Journal](/journal/)\n"),
		"/fm.md":                 {Body: "---\nname: Fenced\naudience: maintainers\n---\n# Fm\n\nSummary.\n", Meta: map[string]string{"tags": "a", "type": "Guide", "version": "3"}},
		"/journal/2026-01-01.md": {Body: "# Day\n\nNotes.\n", Meta: map[string]string{"tags": "journal", "type": "Document"}},
	})
	fm := writes["/fm.md"]
	if fm.Body != "# Fm\n\nSummary.\n" || fm.Version != 3 || fm.Metadata["title"] != "Fenced" || fm.Metadata["audience"] != "maintainers" || fm.Metadata["tags"] != "a" {
		t.Errorf("frontmatter = %+v", fm)
	}
	if day := writes["/journal/2026-01-01.md"]; day.Metadata["type"] != "Journal" || day.Body != "# Day\n\nNotes.\n" {
		t.Errorf("journal entry = %+v", day)
	}
}

// One apply takes at most MaxPlanWrites; the hash binds the batch to the
// versions read.
func TestPlanCapsTheBatchAndHashesIt(t *testing.T) {
	docs := map[string]fakeDoc{"/index.md": hubDoc("# Hub\n\nThe store.\n\n- [Journal](/journal/)\n")}
	for d := range MaxPlanWrites + 1 {
		name := fmt.Sprintf("/journal/2026-%02d-%02d.md", d/28+1, d%28+1)
		docs[name] = fakeDoc{Body: "# Day\n\nNotes.\n", Meta: map[string]string{"tags": "journal", "type": "Document"}}
	}
	r, _ := planOf(t, docs)
	if len(r.Plan.Writes) != MaxPlanWrites || r.Plan.More != 1 || r.Plan.Hash == "" {
		t.Fatalf("writes %d, more %d, hash %q", len(r.Plan.Writes), r.Plan.More, r.Plan.Hash)
	}
	again, _ := planOf(t, docs)
	first := r.Plan.Writes[0].Path
	moved := docs[first]
	moved.Meta = map[string]string{"tags": "journal", "type": "Document", "version": "2"}
	docs[first] = moved
	bumped, _ := planOf(t, docs)
	if again.Plan.Hash != r.Plan.Hash || bumped.Plan.Hash == r.Plan.Hash {
		t.Errorf("hash %s, again %s, after a version bump %s", r.Plan.Hash, again.Plan.Hash, bumped.Plan.Hash)
	}
}
