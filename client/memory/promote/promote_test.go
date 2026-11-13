package promote_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/client/memory/promote"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

const (
	plainDoc    = "# Guidelines\n\nHard rules for code.\n\n## Errors\n\nHandle every one.\n"
	stampedDoc  = "# Guidelines\n\npromoted-via: knowledge\npromoted: mark://team/guidelines.md@v2\n\nHard rules for code.\n\n## Errors\n\nHandle every one.\n"
	stubDoc     = "# Guidelines\n\npromoted-via: knowledge\npromoted: mark://team/guidelines.md@v2\n\nHard rules for code.\n"
	identityKey = "agent"
)

func doc(backend *fetchtest.Client, path string) *docwrite.Doc {
	return &docwrite.Doc{Backend: backend, Host: "h", Path: path, ReadToken: "r", Write: docwrite.SendOnce("w")}
}

func TestMarkersRoundTrip(t *testing.T) {
	m, found := promote.ParseMarkers(stampedDoc)
	if !found || m != (promote.Markers{Via: "knowledge", Target: "mark://team/guidelines.md", Version: 2}) {
		t.Fatalf("markers = %+v, %t", m, found)
	}
	if _, found := promote.ParseMarkers(plainDoc); found || promote.Marked(plainDoc) {
		t.Error("plain body reads as promoted")
	}
	if m, found := promote.ParseMarkers("# X\n\npromoted: /x.md\n"); !found || m.Version != 0 || m.Target != "/x.md" || m.Via != "" {
		t.Errorf("legacy = %+v", m)
	}
	if got := promote.WithMarkers(plainDoc, &m); got != stampedDoc {
		t.Errorf("WithMarkers =\n%s", got)
	}
	// Replacing markers keeps one pair, under the H1, and the body intact.
	m.Version = 3
	if got := promote.WithMarkers(stampedDoc, &m); strings.Count(got, "promoted:") != 1 || !strings.Contains(got, "@v3\n\nHard rules") {
		t.Errorf("replaced =\n%s", got)
	}
	if got := promote.WithoutMarkers(stampedDoc); got != plainDoc {
		t.Errorf("WithoutMarkers =\n%s", got)
	}
	if !promote.IsStub(stubDoc) || promote.IsStub(stampedDoc) || promote.IsStub(plainDoc) {
		t.Error("stub detection")
	}
	if _, _, err := promote.ParseTarget("mark://team/x.md"); err == nil {
		t.Error("target without a version parsed")
	}
}

func TestStageReportsMarkersAndTheLayoutVerdict(t *testing.T) {
	l := layout.Default()
	s := promote.Stage(&promote.Input{Path: "/guidelines.md", Body: stampedDoc, Version: 4, Metadata: map[string]string{"type": "Guideline", "tags": "rules", "importance": "0.9"}}, &l)
	report := s.Report()
	for _, want := range []string{"status: promoted\npath: /guidelines.md\nversion: 4\n", "candidate: type Guideline, importance 0.9\n", "promoted-via: knowledge\npromoted: mark://team/guidelines.md@v2\nmode: full\n", "packet: " + s.Packet + "\n", "tags: rules\n", ">\n# Guidelines\n"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	plain := promote.Stage(&promote.Input{Path: "/journal/2026-10-12.md", Body: plainDoc, Version: 1, Metadata: map[string]string{"type": "Journal"}}, &l)
	if r := plain.Report(); !strings.HasPrefix(r, "status: staged\n") || !strings.Contains(r, "candidate: no (") || strings.Contains(r, "promoted-via") {
		t.Errorf("plain report:\n%s", r)
	}
	if plain.Packet == s.Packet {
		t.Error("packets collide")
	}
}

// A copy lives in another store: URL pins carry, a path pin does not.
func TestStageCarriesPortablePins(t *testing.T) {
	l := layout.Default()
	const repo = "https://github.com/o/r/tree/abcdef0/src"
	s := promote.Stage(&promote.Input{Path: "/plans/x.md", Body: plainDoc, Version: 2, Metadata: map[string]string{"rel-based-on": "/adr/a.md/v3, " + repo + ", mark://team/b.md/v4"}}, &l)
	for _, want := range []string{"rel-based-on: " + repo + ", mark://team/b.md/v4\n", "note: 1 rel-based-on values not carried: only a URL names its source from another store\n"} {
		if r := s.Report(); !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	bare := promote.Stage(&promote.Input{Path: "/plans/x.md", Body: plainDoc, Version: 2}, &l)
	if r := bare.Report(); strings.Contains(r, "rel-based-on") {
		t.Errorf("unpinned source reports pins:\n%s", r)
	}
}

func TestQualifies(t *testing.T) {
	l := layout.Default()
	cases := map[string]struct {
		path string
		meta map[string]string
		want string
	}{
		"adr":        {"/adr/0001-x.md", map[string]string{"type": "Decision"}, "decision record, type Decision"},
		"floor":      {"/x.md", map[string]string{"importance": "0.8"}, "importance 0.8"},
		"under":      {"/x.md", map[string]string{"importance": "0.7", "type": "Note"}, ""},
		"staging":    {"/journal/x.md", map[string]string{"type": "Decision"}, ""},
		"unparsable": {"/x.md", map[string]string{"importance": "NaN"}, ""},
	}
	for name, tc := range cases {
		if got := l.Qualifies(tc.path, tc.meta); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestBackStampFullKeepsTheBodyAndWidensTags(t *testing.T) {
	l := layout.Default()
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"h/g.md": fetchtest.Head(plainDoc, 4, map[string]string{"tags": "rules", "retention": "3", identityKey: "old"})}}
	staged := promote.Stage(&promote.Input{Path: "/g.md", Body: plainDoc, Version: 4}, &l)
	s := promote.Stamp{Markers: promote.Markers{Via: "knowledge", Target: "mark://team/g.md", Version: 1}, Packet: staged.Packet}
	w, err := promote.BackStamp(t.Context(), doc(backend, "/g.md"), map[string]string{identityKey: "me"}, &s)
	if err != nil || w.Version != 1 || w.Mode != promote.ModeFull || w.Note != "" {
		t.Fatalf("stamp = %+v, %v", w, err)
	}
	p := backend.PublishCalls[0]
	if p.ExpectedVersion != 4 || p.Metadata["tags"] != "rules,promoted" || p.Metadata["retention"] != "" || p.Metadata[identityKey] != "me" || !strings.HasPrefix(p.Body, "# Guidelines\n\npromoted-via: knowledge\npromoted: mark://team/g.md@v1\n\nHard rules") {
		t.Errorf("publish = %+v", p)
	}
	if w.Body != p.Body || w.Metadata["version"] != "1" {
		t.Errorf("written = %+v", w)
	}
}

func TestBackStampStubBindsToTheStagedVersion(t *testing.T) {
	l := layout.Default()
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"h/g.md": fetchtest.Head(plainDoc, 4, map[string]string{"description": "Hard rules, in short."})}}
	stale := promote.Stage(&promote.Input{Path: "/g.md", Body: plainDoc, Version: 3}, &l)
	s := promote.Stamp{Markers: promote.Markers{Via: "k", Target: "/t.md", Version: 1}, Stub: true, Packet: stale.Packet}
	if _, err := promote.BackStamp(t.Context(), doc(backend, "/g.md"), nil, &s); err == nil || !strings.Contains(err.Error(), "changed since staging (now v4)") || len(backend.PublishCalls) != 0 {
		t.Fatalf("stale stub = %v", err)
	}
	// A full stamp on a moved document still lands, with a note.
	s.Stub = false
	if w, err := promote.BackStamp(t.Context(), doc(backend, "/g.md"), nil, &s); err != nil || !strings.Contains(w.Note, "changed since staging") {
		t.Fatalf("full on moved = %+v, %v", w, err)
	}
	// The stub keeps the title, the markers and the description as the summary.
	s.Stub, s.Packet = true, promote.Stage(&promote.Input{Path: "/g.md", Body: backend.PublishCalls[0].Body, Version: 1}, &l).Packet
	w, err := promote.BackStamp(t.Context(), doc(backend, "/g.md"), nil, &s)
	if err != nil || w.Mode != promote.ModeStub {
		t.Fatalf("stub = %+v, %v", w, err)
	}
	if got := backend.PublishCalls[1].Body; got != "# Guidelines\n\npromoted-via: k\npromoted: /t.md@v1\n\nHard rules, in short.\n" {
		t.Errorf("stub body =\n%s", got)
	}
}

func TestPlanRefreshByMode(t *testing.T) {
	live := "# Guidelines v2\n\nRules, revised.\n\n## Errors\n\nWrap them.\n"
	in := &promote.Input{Path: "/g.md", Body: stampedDoc, Version: 5}
	for _, tc := range []struct {
		version int
		status  string
	}{{1, promote.RefreshBehind}, {2, promote.RefreshInSync}, {3, promote.RefreshStale}} {
		p, err := promote.PlanRefresh(in, &promote.Refresh{Live: live, LiveVersion: tc.version, Target: "mark://team/guidelines.md"})
		if err != nil || p.Status != tc.status {
			t.Errorf("v%d: %+v, %v", tc.version, p, err)
		}
	}
	p, _ := promote.PlanRefresh(in, &promote.Refresh{Live: live, LiveVersion: 3, Target: "mark://team/guidelines.md"})
	if p.Mode != promote.ModeFull || p.Body != "# Guidelines\n\npromoted-via: knowledge\npromoted: mark://team/guidelines.md@v3\n\nRules, revised.\n\n## Errors\n\nWrap them.\n" || p.Packet == "" {
		t.Errorf("full plan = %+v", p)
	}
	if r := p.Report(); !strings.HasPrefix(r, "status: stale\npath: /g.md\nversion: 5\ntracked: v2\nlive: v3\nmode: full\npacket: "+p.Packet+"\n\n<doc-") {
		t.Errorf("report:\n%s", r)
	}
	stub, _ := promote.PlanRefresh(&promote.Input{Path: "/g.md", Body: stubDoc, Version: 5}, &promote.Refresh{Live: live, LiveVersion: 3, Target: "mark://team/guidelines.md"})
	if stub.Mode != promote.ModeStub || stub.Body != "# Guidelines\n\npromoted-via: knowledge\npromoted: mark://team/guidelines.md@v3\n\nRules, revised.\n" {
		t.Errorf("stub plan =\n%s", stub.Body)
	}
	if _, err := promote.PlanRefresh(in, &promote.Refresh{Live: live, LiveVersion: 3, Target: "mark://other/x.md"}); err == nil {
		t.Error("foreign target accepted")
	}
	if u, _ := promote.PlanRefresh(&promote.Input{Path: "/p.md", Body: plainDoc}, &promote.Refresh{Live: live, LiveVersion: 1}); u.Status != promote.RefreshUnpromoted || !strings.HasPrefix(u.Report(), "status: unpromoted\n") {
		t.Errorf("unpromoted = %+v", u)
	}
}

func TestApplyRefreshWritesAtThePlannedVersionOnly(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"h/g.md": fetchtest.Head(stampedDoc, 5, map[string]string{"tags": "rules"})}}
	p, _ := promote.PlanRefresh(&promote.Input{Path: "/g.md", Body: stampedDoc, Version: 4, Metadata: map[string]string{"tags": "rules"}}, &promote.Refresh{Live: "# G\n\nNew.\n", LiveVersion: 3, Target: "mark://team/guidelines.md"})
	if _, err := promote.ApplyRefresh(t.Context(), doc(backend, "/g.md"), nil, &p); err == nil || !strings.Contains(err.Error(), "moved since v4") {
		t.Fatalf("moved = %v", err)
	}
	p.Version = 5
	w, err := promote.ApplyRefresh(t.Context(), doc(backend, "/g.md"), nil, &p)
	if err != nil || w.Version != 1 || backend.PublishCalls[1].ExpectedVersion != 5 || backend.PublishCalls[1].Metadata["tags"] != "rules,promoted" {
		t.Fatalf("apply = %+v, %v, %+v", w, err, backend.PublishCalls)
	}
	p.Status = promote.RefreshInSync
	if _, err := promote.ApplyRefresh(t.Context(), doc(backend, "/g.md"), nil, &p); err == nil {
		t.Error("in-sync wrote")
	}
}

// A refused write reports the store's reason from the error body, not the
// status or the body's title line.
func TestApplyRefreshReportsTheRefusalReason(t *testing.T) {
	backend := &fetchtest.Client{PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest, Body: protocol.ErrorBody(protocol.StatusBadRequest, "missing required tag axis category")}}, nil
	}}
	p := &promote.Plan{Status: promote.RefreshStale, Path: "/g.md", Version: 5, Body: "# G\n"}
	if _, err := promote.ApplyRefresh(t.Context(), doc(backend, "/g.md"), nil, p); err == nil || err.Error() != "publish /g.md: bad-request: missing required tag axis category" {
		t.Fatalf("refusal = %v", err)
	}
}

func TestExamineAgainstThePolicy(t *testing.T) {
	policy := publishpolicy.Parse("strictness: block\nrequire_tags: domain, category\nrequire_fields: type, user\n")
	d := &promote.Draft{Path: "/x.md", Body: "# X\n\nWhat x settles — now.\n", Metadata: map[string]string{"tags": "domain:a,x"}, Version: 2}
	c := promote.Examine(d, &policy)
	// The server types an untyped document before judging it, so type is not missing.
	if got := strings.Join(c.Missing, "|"); got != "missing tag axis category (an category:<value> tag)" {
		t.Errorf("missing = %q", got)
	}
	allowed := publishpolicy.Parse("strictness: block\nallow_tags.domain: a|b\n")
	if got := gate.Missing("/x.md", map[string]string{"tags": "domain:c"}, "", &allowed); len(got) != 1 || !strings.HasPrefix(got[0], "disallowed tag domain:c (allowed domain values: ") {
		t.Errorf("disallowed = %q", got)
	}
	// The server judges body links against allow_domains; the dry run must too.
	links := publishpolicy.Parse("strictness: block\nallow_domains: github.com\n")
	if got := gate.Missing("/x.md", map[string]string{"tags": "a"}, "see https://evil.io/x\n", &links); len(got) != 1 || got[0] != "link to evil.io outside allow_domains (github.com)" {
		t.Errorf("link = %q", got)
	}
	if r := (&promote.Check{Policy: &links}).Report(); !strings.Contains(r, "policy: strictness block; allow_domains github.com\n") {
		t.Errorf("report policy line = %q", r)
	}
	// A candidate's classifier verdict reaches the reader under its frame.
	flagged := promote.Check{Candidates: []promote.Candidate{{Path: "/k.md", Version: 4, Body: "# K\n", Metadata: map[string]string{"classifier": "injection: role override (judge)"}}}}
	if r := flagged.Report(); !strings.Contains(r, "</doc-") || !strings.Contains(r, ">\nclassifier: injection: role override (judge)\n") {
		t.Errorf("candidate report = %q", r)
	}
	if len(c.Problems) == 0 || c.Packet == "" {
		t.Errorf("check = %+v", c)
	}
	r := c.Report()
	for _, want := range []string{"status: findings\nversion: 2\npolicy: strictness block; require_tags domain, category; require_fields type\n", "findings:\n- missing tag axis category", "- em-dash: 1 em dash", "packet: " + c.Packet + "\n"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	clean := promote.Examine(&promote.Draft{Path: "/x.md", Body: "# X\n\nWhat x settles.\n", Metadata: map[string]string{"tags": "x"}}, nil)
	if !strings.HasPrefix(clean.Report(), "status: ready\nversion: 0\npolicy: none\n") {
		t.Errorf("clean:\n%s", clean.Report())
	}
	if clean.Packet == c.Packet {
		t.Error("packets collide")
	}
	// An unpinned draft is noted once: by the policy's finding when it requires the pin.
	if !strings.Contains(clean.Report(), "note: unpinned: ") {
		t.Errorf("unpinned draft not noted:\n%s", clean.Report())
	}
	pinned := promote.Examine(&promote.Draft{Path: "/x.md", Body: "# X\n\nWhat x settles.\n", Metadata: map[string]string{"tags": "x", "rel-based-on": "https://github.com/o/r/tree/abcdef0"}}, nil)
	requires := publishpolicy.Parse("strictness: block\nrequire_fields: rel-based-on\n")
	required := promote.Examine(&promote.Draft{Path: "/x.md", Body: "# X\n\nWhat x settles.\n", Metadata: map[string]string{"tags": "x"}}, &requires)
	if len(pinned.Notes) != 0 || len(required.Notes) != 0 || !slices.Contains(required.Missing, "missing metadata rel-based-on") {
		t.Errorf("pinned notes = %q, required notes = %q, missing = %q", pinned.Notes, required.Notes, required.Missing)
	}
}

func TestCandidates(t *testing.T) {
	d := &promote.Draft{Path: "/docs/x.md", Body: "# Error Handling\n\n...\n", Metadata: map[string]string{"tags": "errors,go,domain:a,retries"}}
	rows := []lookuptable.Row{
		{Path: "/docs/x.md", Title: "Old X", Tags: "misc"},
		{Path: "/other.md", Title: "error handling", Tags: "x"},
		{Path: "/other.md", Anchor: "sec", Title: "error handling › Sec", Tags: "x"},
		{Path: "/go.md", Title: "Go notes › Retries", Tags: "go, retries"},
		{Path: "/far.md", Title: "Far", Tags: "go"},
	}
	cands, related := promote.Candidates(rows, d, false)
	if len(cands) != 2 || cands[0].Why != "the target path" || cands[1].Why != "same title" || !cands[1].Fetch {
		t.Errorf("by title = %+v", cands)
	}
	if strings.Join(related, "|") != "/go.md: Go notes|/far.md: Far" {
		t.Errorf("related = %v", related)
	}
	cands, _ = promote.Candidates(rows[3:], d, true)
	if len(cands) != 1 || cands[0].Why != "shares tags go, retries" || cands[0].Fetch {
		t.Errorf("by tags = %+v", cands)
	}
	if got := d.SubjectTags(); strings.Join(got, ",") != "retries,errors,go" {
		t.Errorf("subject tags = %v", got)
	}
}

// A marker is a line of the block under the H1: one quoted in an example
// neither marks the document nor is rewritten by a stamp.
func TestMarkersAreReadOnlyUnderTheH1(t *testing.T) {
	example := "## Format\n\n```\npromoted-via: other\npromoted: mark://other/x.md@v9\n```\n\npromoted: /in-prose.md@v4\n"
	plain := "# Promotion\n\nHow a source is stamped.\n\n" + example
	if promote.Marked(plain) || promote.IsPromoted(nil, plain) {
		t.Error("an example marker marked the document")
	}
	if _, found := promote.ParseMarkers(plain); found {
		t.Error("an example marker was parsed as the document's")
	}
	if got := promote.WithoutMarkers(plain); got != plain {
		t.Errorf("an unmarked body changed:\n%s", got)
	}
	m := promote.Markers{Via: "knowledge", Target: "mark://team/promotion.md", Version: 2}
	stamped := promote.WithMarkers(plain, &m)
	if !strings.HasSuffix(stamped, example) || !strings.HasPrefix(stamped, "# Promotion\n\npromoted-via: knowledge\npromoted: mark://team/promotion.md@v2\n\nHow a source") {
		t.Errorf("stamp rewrote the example:\n%s", stamped)
	}
	if got, found := promote.ParseMarkers(stamped); !found || got != m {
		t.Errorf("markers = %+v, want the stamp's, not the example's", got)
	}
	if got := promote.WithoutMarkers(stamped); got != plain {
		t.Errorf("unstamped body differs from the original:\n%s", got)
	}
}

// A setext H1 keeps its underline: the markers go under the heading whole.
func TestMarkersGoUnderASetextH1(t *testing.T) {
	m := promote.Markers{Via: "k", Target: "/t.md", Version: 1}
	got := promote.WithMarkers("Guidelines\n==========\n\nHard rules.\n", &m)
	if got != "Guidelines\n==========\n\npromoted-via: k\npromoted: /t.md@v1\n\nHard rules.\n" {
		t.Errorf("stamped = %q", got)
	}
	if links := promote.Stub(got, ""); links != "# Guidelines\n\nHard rules.\n" {
		t.Errorf("stub = %q", links)
	}
}
