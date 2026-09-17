package doctor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestInventoryFollowsCursorsIntoSubdirectories(t *testing.T) {
	docs := map[string]fakeDoc{}
	for _, p := range []string{"/index.md", "/a.md", "/b.md", "/c.md", "/proj/index.md", "/proj/x.md", "/proj/deep/y.md"} {
		docs[p] = tagged("# T\n\nSummary.\n")
	}
	f := newFake(docs)
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Documents != len(docs) {
		t.Fatalf("inventory found %d of %d documents", r.Documents, len(docs))
	}
	if f.listCalls < 5 {
		t.Fatalf("page size 2 must need several list calls, got %d", f.listCalls)
	}
}

func TestRunRejectsUntrustworthyInventories(t *testing.T) {
	if _, err := Run(context.Background(), newFake(map[string]fakeDoc{}), Options{Scope: "bad slug"}); err == nil {
		t.Fatal("invalid scope must be rejected")
	}
	if _, err := Run(context.Background(), newFake(map[string]fakeDoc{}), Options{Scope: "/"}); err == nil {
		t.Fatal("missing scope must fail the audit")
	}
	f := newFake(map[string]fakeDoc{"/index.md": tagged("# T\n")})
	f.listStatus["/"] = protocol.StatusUnauthorized
	if _, err := Run(context.Background(), f, Options{Scope: "/"}); err == nil {
		t.Fatal("unauthorized list must fail the audit")
	}
	stuck := newFake(map[string]fakeDoc{"/index.md": tagged("# T\n"), "/a.md": tagged("# A\n"), "/b.md": tagged("# B\n")})
	stuck.stuck = true
	if _, err := Run(context.Background(), stuck, Options{Scope: "/"}); err == nil {
		t.Fatal("a cursor that does not advance must fail the audit")
	}
	unauth := newFake(map[string]fakeDoc{"/index.md": {Body: "# T\n", Status: protocol.StatusUnauthorized}})
	if _, err := Run(context.Background(), unauth, Options{Scope: "/"}); err == nil {
		t.Fatal("unauthorized fetch must fail the audit")
	}
}

func TestRunEnforcesDocumentBound(t *testing.T) {
	docs := map[string]fakeDoc{}
	for i := 0; i <= MaxDocuments; i++ {
		docs[fmt.Sprintf("/d%05d.md", i)] = tagged("# T\n")
	}
	f := newFake(docs)
	f.pageSize = 1000
	if _, err := Run(context.Background(), f, Options{Scope: "/"}); err == nil || !strings.Contains(err.Error(), "documents") {
		t.Fatalf("document bound must fail the audit, got %v", err)
	}
}

func TestLinkChecks(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md":         tagged("# Hub\n\n- [a](/a.md)\n- [gone](/gone.md)\n- [journal](/journal/)\n- [other](mark://other.example/x.md)\n"),
		"/a.md":             tagged("# A\n\nSummary.\n\n[b](b.md) [outside](/outside.md) [flaky](/flaky.md)\n"),
		"/b.md":             tagged("# B\n\nSummary.\n"),
		"/lonely.md":        tagged("# L\n\nSummary.\n"),
		"/journal/1.md":     tagged("# J1\n"),
		"/journal/2.md":     tagged("# J2\n"),
		"/outside.md":       tagged("# O\n"),
		"/proj/index.md":    tagged("# P\n\n- [n](nested/x.md)\n"),
		"/proj/nested/x.md": tagged("# X\n\nSummary.\n"),
	})
	f.missing["/flaky.md"] = protocol.StatusServerError
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckBrokenLinks); len(got) != 1 || !strings.Contains(got[0], "/index.md: -> /gone.md") {
		t.Fatalf("broken links: %v", got)
	}
	if got := findings(r, CheckStaleIndex); len(got) != 1 {
		t.Fatalf("a hub's broken link is a stale index entry: %v", got)
	}
	if got := findings(r, CheckUnresolved); len(got) != 1 || !strings.Contains(got[0], "/flaky.md (fetch: server-error)") {
		t.Fatalf("a non not-found failure is unresolved: %v", got)
	}
	if got := findings(r, CheckOrphans); len(got) != 2 || !strings.Contains(got[0], "/lonely.md") || !strings.Contains(got[1], "/proj/index.md") {
		t.Fatalf("orphans must exclude the root hub, linked documents and directory-linked journal entries: %v", got)
	}
	if got := findings(r, CheckMissingHub); len(got) != 1 || !strings.HasPrefix(got[0], "/journal/:") {
		t.Fatalf("missing hub: %v", got)
	}
}

func TestLinkConfirmationBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Hub\n\n")
	for i := range MaxLinkConfirms + 3 {
		fmt.Fprintf(&b, "- [m](/missing-%d.md)\n", i)
	}
	f := newFake(map[string]fakeDoc{"/index.md": tagged(b.String())})
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckBrokenLinks); len(got) != MaxLinkConfirms {
		t.Fatalf("confirmed %d links, want %d", len(got), MaxLinkConfirms)
	}
	if len(r.Coverage) != 1 || !strings.Contains(r.Coverage[0], "3 links unconfirmed") {
		t.Fatalf("links past the budget are a coverage gap, not findings: %v", r.Coverage)
	}
}

func TestMetadataTitleAndADRChecks(t *testing.T) {
	adr := func(n string) fakeDoc {
		return fakeDoc{Body: "# " + n + ". Title\n\nSummary.\n", Meta: map[string]string{"tags": "adr", "type": "Decision"}}
	}
	f := newFake(map[string]fakeDoc{
		"/index.md":                {Body: "# Hub\n\n- [x](/x.md)\n- [adr](/adr/)\n- [t](/typed.md)\n- [u](/untitled.md)\n- [d1](/dup1.md)\n- [d2](/dup2.md)\n- [g](/generic.md)\n", Meta: map[string]string{"tags": "index"}},
		"/x.md":                    {Body: "# X\n\nSummary.\n"},
		"/typed.md":                tagged("# T\n\nSummary.\n"),
		"/untitled.md":             {Body: "no heading here\n", Meta: map[string]string{"tags": "x", "type": "Guide"}},
		"/generic.md":              {Body: "# G\n\nSummary.\n", Meta: map[string]string{"tags": "x", "type": "Document"}},
		"/dup1.md":                 {Body: "# D\n\nSame.\n", Meta: map[string]string{"tags": "x", "type": "Guide", "content-hash": "sha256-aa"}},
		"/dup2.md":                 {Body: "# D\n\nSame.\n", Meta: map[string]string{"tags": "x", "type": "Guide", "content-hash": "sha256-aa"}},
		"/adr/0001-first.md":       adr("0001"),
		"/adr/0001-first-again.md": adr("0001"),
		"/adr/0003-third.md":       adr("0003"),
	})
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckUntagged); len(got) != 1 || !strings.HasPrefix(got[0], "/x.md") {
		t.Fatalf("untagged: %v", got)
	}
	got := findings(r, CheckUntyped)
	if len(got) != 2 || !strings.HasPrefix(got[0], "/generic.md: type=Document") || !strings.HasPrefix(got[1], "/x.md: type missing") {
		t.Fatalf("untyped must flag missing and Document, never index.md: %v", got)
	}
	if got := findings(r, CheckUntitled); len(got) != 1 || !strings.HasPrefix(got[0], "/untitled.md") {
		t.Fatalf("untitled: %v", got)
	}
	if got := findings(r, CheckDuplicates); len(got) != 1 || !strings.Contains(got[0], "/dup1.md: identical body at /dup2.md") {
		t.Fatalf("duplicates: %v", got)
	}
	got = findings(r, CheckADRSequence)
	if len(got) != 2 || !strings.Contains(got[0], "0001 used by") || !strings.Contains(got[1], "missing 0002") {
		t.Fatalf("adr sequence: %v", got)
	}
}

func TestShapeStyleAndFrontmatterChecks(t *testing.T) {
	big := "# Big\n\nSummary.\n\n" + strings.Repeat("text line\n", 1000)
	f := newFake(map[string]fakeDoc{
		"/index.md": {Body: "# Hub\n\n- [big](/big.md)\n- [s](/shape.md)\n- [fm](/fm.md)\n", Meta: map[string]string{"tags": "index"}},
		"/big.md":   tagged(big),
		"/shape.md": tagged("# S\n## Now\n\nSummary later.\n\n## Done COMPLETED\n\nText \u2014 with dash.\n\n## Now\n"),
		"/fm.md":    tagged("---\nversion: 3\n---\n# FM\n\nSummary.\n"),
	})
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckOversized); len(got) != 1 || !strings.HasPrefix(got[0], "/big.md") {
		t.Fatalf("oversized: %v", got)
	}
	got := findings(r, CheckDocumentShape)
	if len(got) != 2 || !strings.Contains(got[0], "no summary") || !strings.Contains(got[1], "carry status") {
		t.Fatalf("document shape: %v", got)
	}
	got = findings(r, CheckStyle)
	if len(got) != 2 || !strings.Contains(got[0], "1 em dash") || !strings.Contains(got[1], "duplicate headings") {
		t.Fatalf("style: %v", got)
	}
	if got := findings(r, CheckFrontmatter); len(got) != 1 || !strings.HasPrefix(got[0], "/fm.md") {
		t.Fatalf("frontmatter: %v", got)
	}
}

func TestReferenceChecks(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md":               {Body: "# Hub\n\n- [adr](/adr/)\n- [n](/notes.md)\n- [p](/proj/)\n", Meta: map[string]string{"tags": "index"}},
		"/adr/0001-one.md":        tagged("# 0001. One\n\nSummary.\n"),
		"/adr/0002-two.md":        tagged("# 0002. Two\n\nSupersedes ADR 0001 and mentions ADR 0002 itself.\n\n[link](/adr/0001-one.md)\n"),
		"/notes.md":               tagged("# Notes\n\nSee ADR-0001 and ADR 0009.\n\n```\nADR 0007 in a fence\n```\n"),
		"/proj/index.md":          {Body: "# P\n\n- [a](/proj/adr/)\n", Meta: map[string]string{"tags": "index"}},
		"/proj/adr/0001-local.md": tagged("# 0001. Local\n\nSummary.\n"),
		"/proj/thoughts.md":       tagged("# T\n\nADR 0001 here means the project's own.\n"),
	})
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	got := findings(r, CheckReferences)
	want := []string{
		"/notes.md: \"ADR-0001\" unlinked: /adr/0001-one.md exists",
		"/notes.md: \"ADR 0009\" dangling",
		"/proj/thoughts.md: \"ADR 0001\" unlinked: /proj/adr/0001-local.md exists",
	}
	if len(got) != len(want) {
		t.Fatalf("references: %v", got)
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Fatalf("reference %d: got %q, want prefix %q", i, got[i], w)
		}
	}
}

func TestDeepLostMetadata(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md": {Body: "# Hub\n\n- [j](/j.md)\n- [n](/never.md)\n- [b](/broken.md)\n", Meta: map[string]string{"tags": "index"}},
		"/j.md": {Body: "# J\n\nSummary.\n", Meta: map[string]string{"type": "Journal"}, Versions: map[int]fakeDoc{
			1: {Meta: map[string]string{"tags": "journal,x", "importance": "0.6", "type": "Journal"}},
			2: {Meta: map[string]string{"type": "Journal"}},
		}},
		"/never.md":  {Body: "# N\n\nSummary.\n", Versions: map[int]fakeDoc{1: {Meta: map[string]string{}}}},
		"/broken.md": {Body: "# B\n\nSummary.\n", Meta: map[string]string{"chain-valid": "false"}, Versions: map[int]fakeDoc{1: {Meta: map[string]string{"tags": "x"}}}},
	})
	r, err := Run(context.Background(), f, Options{Scope: "/", Deep: true})
	if err != nil {
		t.Fatal(err)
	}
	got := findings(r, CheckLostMetadata)
	if len(got) != 3 {
		t.Fatalf("lost metadata: %v", got)
	}
	if !strings.Contains(got[0], "/broken.md: inconclusive") || !strings.Contains(got[1], "/j.md: candidate: tagged at v1 (2 tags, importance 0.6, type Journal), none at v3") || !strings.Contains(got[2], "/never.md: untagged through") {
		t.Fatalf("lost metadata details: %v", got)
	}
}

func TestReportMarkdownAndClean(t *testing.T) {
	f := newFake(map[string]fakeDoc{"/index.md": {Body: "# Hub\n\n- [a](/a.md)\n", Meta: map[string]string{"tags": "index"}}, "/a.md": tagged("# A\n\nSummary.\n")})
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != 0 || len(r.Coverage) != 0 {
		t.Fatalf("expected a clean report, got %+v %v", r.Findings, r.Coverage)
	}
	md := r.Markdown()
	for _, want := range []string{"## / hygiene report  (2 docs)", "2 docs, 0 findings", "### Coverage\n- complete"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
	if _, err := r.JSON(); err != nil {
		t.Fatal(err)
	}
	scoped, err := Run(context.Background(), f, Options{Scope: "/nope/"})
	if err == nil || scoped != nil {
		t.Fatalf("a project scope that does not exist fails the audit, got %v", err)
	}
	drift := &Report{Scope: "/", Findings: []Finding{{Check: CheckTokenDrift, Path: "provision verify-auth", Detail: "token drift: x", Fix: "fix it"}}}
	if md := drift.Markdown(); !strings.Contains(md, "### Token drift (1)\n- provision verify-auth: token drift: x; fix: fix it") {
		t.Fatalf("token drift renders as a check section:\n%s", md)
	}
}
