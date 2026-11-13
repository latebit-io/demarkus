package audit

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

func TestDriftChecks(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md":       {Body: "# Hub\n\n- [a](/adr/a.md)\n- [r](/notes/r.md)\n- [o](/notes/old.md)\n", Meta: map[string]string{"tags": "hub"}},
		"/adr/a.md":       {Body: "# A\n\nSummary of A.\n", Meta: map[string]string{"tags": "adr", "version": "5"}},
		"/notes/r.md":     {Body: "# R\n\nSummary of R.\n", Meta: map[string]string{"tags": "notes", "user": "alice", "rel-based-on": "../adr/a.md/v3, mark://kb/adr/a.md/v5, /adr/a.md/v9, /gone.md/v1"}},
		"/notes/old.md":   {Body: "# Old\n\nSummary of Old.\n", Meta: map[string]string{"tags": "notes", "user": "bob", "owner": "team-docs", "rel-based-on": "/adr/a.md/v5, https://github.com/o/r/commit/abcdef0, mark://elsewhere/x.md/v1"}},
		"/notes/scope.md": {Body: "# S\n\nSummary of S.\n", Meta: map[string]string{"tags": "notes", "rel-based-on": "/adr/a.md/v1, /reports/out.md/v1"}},
		// A caller's own output, skipped: no document, no finding, no count.
		"/reports/out.md": {Body: "no heading — nobody links here\n", Meta: map[string]string{"rel-based-on": "/adr/a.md/v1"}},
	})
	f.host = "kb"
	skip := []string{"/reports/out.md"}
	r, err := Run(context.Background(), f, Options{Scope: "/", Skip: skip})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/notes/r.md: based on /adr/a.md, which is at v5, past the pinned v3",
		"/notes/r.md: based on /adr/a.md, which has no v9; it is at v5",
		"/notes/r.md: based on /gone.md, which is not a live document here",
		"/notes/scope.md: based on /adr/a.md, which is at v5, past the pinned v1",
	}
	if got := findings(r, CheckDrift); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("drift findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A pin on the skipped document is unchecked, never a missing source.
	if len(r.Coverage) != 1 || !strings.HasPrefix(r.Coverage[0], "1 rel-based-on pins on documents outside the scope or skipped") {
		t.Errorf("coverage = %q, want only the pin on the skipped document: repository and other-world pins are by design", r.Coverage)
	}
	// The drift-only audit counts and finds what the full one does.
	only, err := Drift(context.Background(), f, Options{Scope: "/", Skip: skip})
	if err != nil {
		t.Fatal(err)
	}
	if only.Freshness != r.Freshness || only.Documents != r.Documents || strings.Join(findings(only, CheckDrift), "\n") != strings.Join(want, "\n") {
		t.Errorf("drift-only audit = %+v, %d documents, %q; want the full audit's", only.Freshness, only.Documents, findings(only, CheckDrift))
	}
	// The hub is navigation; the repository and other-world pins are counted, not checked.
	if want := (Freshness{Eligible: 4, Pinned: 3, Owned: 2, Pins: 9, PinsChecked: 6, DocumentsChecked: 3, DocumentsDrifted: 2, DocumentsUnowned: 1, MedianBehind: 4, MaxBehind: 4}); r.Freshness != want {
		t.Errorf("freshness = %+v, want %+v", r.Freshness, want)
	}
	section := "\n### Freshness\n- pinned: 3 of 4 documents (75%)\n- owned: 2 of 4 documents (50%)\n- pins: 6 of 9 checked\n- drifted: 2 of 3 documents checked; median 4 versions behind, most 4; 1 with no owner\n\n### Coverage\n"
	md := r.Markdown()
	if !strings.Contains(md, section) {
		t.Errorf("report missing the freshness section:\n%s", md)
	}
	// Drift goes to the last verified writer; a document nobody wrote under an identity names none.
	if !strings.Contains(md, "- /notes/r.md: based on /adr/a.md, which is at v5, past the pinned v3 (owner alice); fix: ") || strings.Contains(md, "/notes/scope.md: based on /adr/a.md, which is at v5, past the pinned v1 (owner") {
		t.Errorf("drift bullets carry the wrong owners:\n%s", md)
	}

	r, err = Run(context.Background(), f, Options{Scope: "/notes/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckDrift); len(got) != 0 {
		t.Errorf("drift findings outside the source's scope: %q", got)
	}
	if len(r.Coverage) != 1 || !strings.HasPrefix(r.Coverage[0], "7 rel-based-on pins on documents outside the scope or skipped") {
		t.Errorf("coverage = %q", r.Coverage)
	}
	// Nothing in scope is a source, so nothing is checked and no drift line shows.
	if want := (Freshness{Eligible: 3, Pinned: 3, Owned: 2, Pins: 9}); r.Freshness != want || strings.Contains(r.Markdown(), "- drifted:") {
		t.Errorf("freshness = %+v, want %+v and no drift line", r.Freshness, want)
	}
}

func TestPolicyChecksHonourExemptions(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md":                       {Body: "# Hub\n\n- [a](/a.md)\n- [g](/graph/g.md)\n", Meta: map[string]string{"tags": "hub"}},
		"/.well-known/demarkus/policy.md": {Body: "# Policy\n\nrequire_fields: description\nexempt: /graph/**\n", Meta: map[string]string{"tags": "policy"}},
		"/a.md":                           {Body: "# A\n\nSummary of A.\n", Meta: map[string]string{"tags": "a", "rel-based-on": "/graph/g.md"}},
		"/graph/g.md":                     {Body: "# G\n\nSummary of G.\n", Meta: map[string]string{"tags": "graph"}},
	})
	floor := publishpolicy.Policy{RequiredFields: []string{"owner"}, Exempt: []string{"/graph/**"}}
	r, err := Run(context.Background(), f, Options{Scope: "/", Floor: &floor})
	if err != nil {
		t.Fatal(err)
	}
	// /graph/g.md answers to neither layer's fields; the baseline still
	// judges every path.
	want := []string{
		"/a.md: rel-based-on value /graph/g.md is not a versioned permalink (strictness warn)",
		"/a.md: missing required metadata owner (strictness warn)",
		"/a.md: missing required metadata description (strictness warn)",
		"/index.md: missing required metadata owner (strictness warn)",
		"/index.md: missing required metadata description (strictness warn)",
	}
	if got := findings(r, CheckPolicy); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("policy findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A path both layers exempt and the hub expect no pin; a value naming no version is not one.
	if want := (Freshness{Eligible: 1, Pins: 1}); r.Freshness != want {
		t.Errorf("freshness = %+v, want %+v", r.Freshness, want)
	}
}

// The owner key names who keeps a document current when the last writer does not.
func TestOwnerPrefersTheOwnerKey(t *testing.T) {
	for want, meta := range map[string]map[string]string{
		"team-docs": {"owner": " team-docs ", "user": "bob"},
		"bob":       {"user": "bob"},
		"":          {"agent": "claude-code"},
	} {
		if got := ownerOf(meta); got != want {
			t.Errorf("owner(%v) = %q, want %q", meta, got, want)
		}
	}
}

// A source whose read failed is unknown, not gone: no finding, a coverage note.
func TestDriftLeavesAnUnreadSourceUnjudged(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md": {Body: "# Hub\n\n- [a](/a.md)\n- [s](/s.md)\n- [g](/g.md)\n", Meta: map[string]string{"tags": "hub"}},
		"/s.md":     {Status: protocol.StatusServerError},
		"/a.md":     {Body: "# A\n\nSummary of A.\n", Meta: map[string]string{"tags": "a", "rel-based-on": "/s.md/v1, /gone.md/v1"}},
	})
	r, err := Drift(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckDrift); len(got) != 1 || got[0] != "/a.md: based on /gone.md, which is not a live document here" {
		t.Errorf("drift findings = %q, want only the source that is gone", got)
	}
	if r.Freshness.PinsChecked != 1 || !slices.ContainsFunc(r.Coverage, func(c string) bool {
		return strings.HasPrefix(c, "1 rel-based-on pins on documents that could not be read")
	}) {
		t.Errorf("pins checked = %d, coverage = %q; want one checked and the unread pin noted", r.Freshness.PinsChecked, r.Coverage)
	}
}
