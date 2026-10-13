package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/stylerules"
)

func TestPolicyChecksUnderAFloor(t *testing.T) {
	policy := "# Policy\n\nstrictness: warn\nallow_tags.category: guide|howto|governance\n"
	f := newFake(map[string]fakeDoc{
		"/index.md":                       {Body: "# Hub\n\n- [a](/a.md)\n- [b](/b.md)\n- [c](/c.md)\n", Meta: map[string]string{"tags": "category:guide"}},
		"/.well-known/demarkus/policy.md": {Body: policy, Meta: map[string]string{"tags": "category:governance", "type": "Reference"}},
		"/a.md":                           {Body: "# A\n\nSummary of A.\n", Meta: map[string]string{"tags": "category:guide", "type": "Guide"}},
		"/b.md":                           {Body: "# B\n\nSummary of B.\n", Meta: map[string]string{"tags": "category:howto", "type": "Guide"}},
		"/c.md":                           {Body: "# C\n\nSummary of C.\n", Meta: map[string]string{"tags": "topic", "importance": "7"}},
	})
	floor := publishpolicy.Policy{Strictness: publishpolicy.Block, RequiredTagAxes: []string{"category"}, RequiredFields: []string{"type"},
		AllowedTagValues: map[string][]string{"category": {"guide", "decision", "governance"}}}
	r, err := Run(context.Background(), f, Options{Scope: "/", Floor: &floor})
	if err != nil {
		t.Fatal(err)
	}
	got := findings(r, CheckPolicy)
	want := []string{
		"/b.md: tag category:howto is outside the allowed values for category (guide|governance) (strictness block)",
		"/c.md: importance \"7\" is not a number in [0,1] (strictness block)",
		"/c.md: missing required tag axis category: (strictness block)",
		"/c.md: missing required metadata type (strictness block)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("policy findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	got = findings(r, CheckPolicyFloor)
	if len(got) != 2 || !strings.Contains(got[0], "strictness warn is below the floor's block") || !strings.Contains(got[1], "allow_tags.category adds howto") {
		t.Fatalf("floor findings: %v", got)
	}
	// Without a floor the world's own warn policy allows howto and asks for nothing.
	r, err = Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckPolicy); len(got) != 1 || !strings.HasPrefix(got[0], "/c.md: importance") {
		t.Fatalf("policy findings without a floor: %v", got)
	}
	if got := findings(r, CheckPolicyFloor); len(got) != 0 {
		t.Fatalf("floor findings without a floor: %v", got)
	}
}

func TestPolicyUnreadableSkipsTheCheck(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md": {Body: "# Hub\n\n- [a](/a.md)\n", Meta: map[string]string{"tags": "index"}},
		"/a.md":     {Body: "# A\n\nSummary.\n", Meta: map[string]string{"tags": "t", "importance": "9"}},
	})
	f.missing[publishpolicy.DocumentPath] = "error"
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findings(r, CheckPolicy); len(got) != 0 {
		t.Errorf("policy findings with an unreadable policy: %v", got)
	}
	if len(r.Coverage) != 1 || !strings.Contains(r.Coverage[0], "policy checks skipped") {
		t.Errorf("coverage = %v", r.Coverage)
	}
}

func TestContentAndStyleGuideChecks(t *testing.T) {
	stale := "# Style Guide\n\nProse.\n\n" + strings.Replace(stylerules.Markdown(), "- `h1`", "- `heading-one`", 1) + "\n"
	f := newFake(map[string]fakeDoc{
		"/index.md":                      {Body: "# Hub\n\n- [k](/k.md)\n- [o](/o.md)\n- [p](/p.md)\n", Meta: map[string]string{"tags": "index"}},
		"/.well-known/demarkus/style.md": {Body: stale, Meta: map[string]string{"tags": "style", "type": "Guide"}},
		"/k.md":                          tagged("# K\n\nSummary.\n\n```\n-----BEGIN RSA PRIVATE KEY-----\n```\n"),
		"/o.md":                          {Body: "# O\n\nSummary.\n\nSIN 046 454 286\n", Meta: map[string]string{"tags": "t", "type": "Guide", "content-check-override": "test fixture"}},
		"/p.md":                          tagged("# P\n\nSummary.\n\nmail pat@gmail.com\n"),
	})
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	// Email stays unchecked: the doctor has no work domains to judge by.
	got := findings(r, CheckContent)
	want := []string{
		"/k.md: secret/pem-private-key at line 6",
		"/o.md: pii/canadian-sin at line 5 (override: test fixture)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("content findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range r.Findings {
		if f.Check == CheckContent && (strings.Contains(f.Detail, "RSA") || strings.Contains(f.Detail, "046") || strings.Contains(f.Detail, "gmail")) {
			t.Errorf("finding echoes the value: %q", f.Detail)
		}
	}
	if got := findings(r, CheckStyleGuide); len(got) != 1 || !strings.Contains(got[0], "differs from the gate's") {
		t.Fatalf("style guide: %v", got)
	}
}

func TestContentOverrideNeverCoversASecret(t *testing.T) {
	f := newFake(map[string]fakeDoc{
		"/index.md": {Body: "# Hub\n\n- [k](/k.md)\n", Meta: map[string]string{"tags": "index"}},
		"/k.md":     {Body: "# K\n\nSummary.\n\n-----BEGIN RSA PRIVATE KEY-----\n", Meta: map[string]string{"tags": "t", "type": "Guide", "content-check-override": "legacy"}},
	})
	r, err := Run(context.Background(), f, Options{Scope: "/"})
	if err != nil {
		t.Fatal(err)
	}
	got := findings(r, CheckContent)
	if len(got) != 1 || got[0] != "/k.md: secret/pem-private-key at line 5" {
		t.Fatalf("content findings: %v", got)
	}
}
