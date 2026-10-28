package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

func TestDriftChecks(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md":       {Body: "# Hub\n\n- [a](/adr/a.md)\n- [r](/notes/r.md)\n- [o](/notes/old.md)\n", Meta: map[string]string{"tags": "hub"}},
		"/adr/a.md":       {Body: "# A\n\nSummary of A.\n", Meta: map[string]string{"tags": "adr", "version": "5"}},
		"/notes/r.md":     {Body: "# R\n\nSummary of R.\n", Meta: map[string]string{"tags": "notes", "rel-based-on": "../adr/a.md/v3, mark://kb/adr/a.md/v5, /adr/a.md/v9, /gone.md/v1"}},
		"/notes/old.md":   {Body: "# Old\n\nSummary of Old.\n", Meta: map[string]string{"tags": "notes", "rel-based-on": "/adr/a.md/v5, https://github.com/o/r/commit/abcdef0, mark://elsewhere/x.md/v1"}},
		"/notes/scope.md": {Body: "# S\n\nSummary of S.\n", Meta: map[string]string{"tags": "notes", "rel-based-on": "/adr/a.md/v1"}},
	})
	f.host = "kb"
	r, err := Run(context.Background(), f, Options{Scope: "/"})
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
	if len(r.Coverage) != 0 {
		t.Errorf("coverage = %q, want none: repository and other-world pins are by design", r.Coverage)
	}

	r, err = Run(context.Background(), f, Options{Scope: "/notes/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckDrift); len(got) != 0 {
		t.Errorf("drift findings outside the source's scope: %q", got)
	}
	if len(r.Coverage) != 1 || !strings.HasPrefix(r.Coverage[0], "6 rel-based-on pins on documents outside the scope") {
		t.Errorf("coverage = %q", r.Coverage)
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
}
