package curate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

func TestCleanDocumentProposesNothing(t *testing.T) {
	p := Propose(&Input{Path: "/a.md", Body: "# A\n\nWhat A settles for its readers.\n\n## One\n\nx\n", Version: 2, Metadata: map[string]string{"tags": "a"}})
	if p.Kind != KindClean || len(p.Writes) != 0 || p.Hash != "" {
		t.Fatalf("proposal = %+v", p)
	}
	if r := p.Report(nil, nil); !strings.Contains(r, "status: clean\npath: /a.md\nversion: 2\n") || !strings.Contains(r, "- all checks pass") {
		t.Errorf("report:\n%s", r)
	}
}

func TestEditFixesWhatTheRulesImply(t *testing.T) {
	body := "---\nname: Doc\ntags: x\n---\n# Doc\n\n## Step 3 (DONE 2026-10-12)\n\nShipped — finally.\n\n## Step 4 #572\n\ntext\n"
	in := &Input{Path: "/doc.md", Body: body, Version: 4,
		Metadata: map[string]string{"tags": "a", "description": "What the doc settles for its readers today.", "agent": "x", "retention": "2"}}
	p := Propose(in)
	if p.Kind != KindEdit || len(p.Writes) != 1 || p.Hash == "" {
		t.Fatalf("proposal = %+v", p)
	}
	w := p.Writes[0]
	want := "# Doc\n\nWhat the doc settles for its readers today.\n\n## Step 3\n\nStatus: DONE 2026-10-12\n\nShipped, finally.\n\n## Step 4\n\nStatus: #572\n\ntext\n"
	if w.Path != "/doc.md" || w.Body != want {
		t.Errorf("body:\n%s\nwant:\n%s", w.Body, want)
	}
	// The fence's keys land in metadata unless the document already has them; identity and retention never carry.
	if w.Metadata["title"] != "Doc" || w.Metadata["tags"] != "a" || w.Metadata["agent"] != "" || w.Metadata["retention"] != "" {
		t.Errorf("metadata = %v", w.Metadata)
	}
	if len(p.Unfixed) != 0 {
		t.Errorf("unfixed = %v", p.Unfixed)
	}
	moves := p.Moves([]Inbound{{Source: "/other.md", Anchor: "step-3-done-2026-10-12"}, {Source: "/other.md", Anchor: "missing"}})
	if len(moves) != 1 || moves[0] != (Move{Source: "/other.md", From: "/doc.md#step-3-done-2026-10-12", To: "/doc.md#step-3"}) {
		t.Errorf("moves = %+v", moves)
	}
	if again := Propose(in); again.Hash != p.Hash {
		t.Error("hash is not stable")
	}
	in.Version++
	if again := Propose(in); again.Hash == p.Hash {
		t.Error("hash ignores the version")
	}
	r := p.Report(moves, []string{"partial"})
	for _, s := range []string{"status: edit\n", "## Proposal: edit\n", "- frontmatter fence removed; metadata keys: title", "Inbound links to repoint:\n\n- /other.md: /doc.md#step-3-done-2026-10-12 -> /doc.md#step-3", "\nnote: partial\n", "proposal: " + p.Hash, "=== /doc.md (description: What the doc settles for its readers today.; tags: a; title: Doc)\n# Doc\n"} {
		if !strings.Contains(r, s) {
			t.Errorf("report missing %q:\n%s", s, r)
		}
	}
}

func TestReportWhenNothingIsMechanical(t *testing.T) {
	p := Propose(&Input{Path: "/d.md", Body: "# D\n\nD is a page about D.\n\n## Same\n\nx\n\n## Same\n\ny\n"})
	if p.Kind != KindReport || len(p.Writes) != 0 || len(p.Unfixed) != 0 || len(p.Problems) < 2 {
		t.Fatalf("proposal = %+v", p)
	}
	if r := p.Report(nil, nil); !strings.Contains(r, "need the agent") {
		t.Errorf("report:\n%s", r)
	}
}

func bigDoc() string {
	var b strings.Builder
	b.WriteString("# Big plan\n\nWhat the plan settles, for maintainers.\n\n## Goal\n\nOne short frame section.\n\n")
	for _, name := range []string{"Design", "Steps", "Measures"} {
		b.WriteString("## " + name + "\n\nFirst sentence of " + strings.ToLower(name) + ". Second.\n\n### " + name + " detail\n\n")
		for range 40 {
			b.WriteString("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore.\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestSplitMovesHeadingsVerbatimAndKeepsTheFrame(t *testing.T) {
	in := &Input{Path: "/plans/big.md", Body: bigDoc(), Version: 7,
		Metadata: map[string]string{"tags": "plan,steps,design,unrelated,status:active", "importance": "0.8", "type": "Plan", "title": "Big plan", "user": "u"}}
	p := Propose(in)
	if p.Kind != KindSplit || len(p.Writes) != 4 {
		t.Fatalf("kind %s writes %d: %+v", p.Kind, len(p.Writes), p.Unfixed)
	}
	topic := p.Writes[1]
	summary := "The Steps section of Big plan, split into its own document."
	if topic.Path != "/plans/big/steps.md" || !strings.HasPrefix(topic.Body, "# Big plan: Steps\n\n"+summary+"\n\nPart of [Big plan](/plans/big.md).\n\n## Steps\n\nFirst sentence of steps. Second.\n\n### Steps detail\n") {
		t.Errorf("topic = %s\n%s", topic.Path, topic.Body[:200])
	}
	// Axis tags carry so the topic meets the write policy its parent met.
	if topic.Metadata["tags"] != "big,steps,status:active" || topic.Metadata["description"] != summary || topic.Metadata["importance"] != "0.7" || topic.Metadata["type"] != "Plan" || topic.Metadata["title"] != "" || topic.Metadata[neardup.CuratedFromKey] != "/plans/big.md" {
		t.Errorf("topic metadata = %v", topic.Metadata)
	}
	hub := p.Writes[3]
	wantHub := "# Big plan\n\nWhat the plan settles, for maintainers.\n\n## Topics\n\n- [Design](/plans/big/design.md): First sentence of design.\n- [Steps](/plans/big/steps.md): First sentence of steps.\n- [Measures](/plans/big/measures.md): First sentence of measures.\n\n## Goal\n\nOne short frame section.\n"
	if hub.Path != "/plans/big.md" || hub.Body != wantHub {
		t.Errorf("hub:\n%s\nwant:\n%s", hub.Body, wantHub)
	}
	// Tags only a topic mentions leave the hub; the rest stay, whether the hub mentions them or nobody does.
	if hub.Metadata["tags"] != "plan,unrelated,status:active" || hub.Metadata["importance"] != "0.8" || hub.Metadata["title"] != "Big plan" || hub.Metadata["user"] != "" {
		t.Errorf("hub metadata = %v", hub.Metadata)
	}
	moves := p.Moves([]Inbound{{Source: "/index.md", Anchor: "steps-detail"}, {Source: "/x.md", Anchor: "goal"}})
	if len(moves) != 1 || moves[0] != (Move{Source: "/index.md", From: "/plans/big.md#steps-detail", To: "/plans/big/steps.md#steps-detail"}) {
		t.Errorf("moves = %+v", moves)
	}
	if len(p.Unfixed) != 0 {
		t.Errorf("unfixed = %v", p.Unfixed)
	}
}

// Topics carry what the policy requires and the parent has; what neither
// supplies is named before apply. Axis tags stay on the hub even when only
// a moved section spells one out.
func TestSplitMeetsTheWritePolicy(t *testing.T) {
	body := strings.Replace(bigDoc(), "First sentence of steps.", "First sentence of steps, status:active.", 1)
	policy := &publishpolicy.Policy{Strictness: publishpolicy.Block, RequiredTagAxes: []string{"status"}, RequiredFields: []string{"owner", "source", "user"}}
	in := &Input{Path: "/plans/big.md", Body: body, Version: 7, Policy: policy,
		Metadata: map[string]string{"tags": "plan,status:active", "type": "Plan", "owner": "team-a", "local-id": "p-1"}}
	p := Propose(in)
	if p.Kind != KindSplit {
		t.Fatalf("kind %s: %v", p.Kind, p.Unfixed)
	}
	topic, hub := p.Writes[1], p.Writes[len(p.Writes)-1]
	if topic.Metadata["owner"] != "team-a" || topic.Metadata["local-id"] != "" || topic.Metadata["user"] != "" {
		t.Errorf("topic metadata = %v", topic.Metadata)
	}
	if hub.Metadata["tags"] != "plan,status:active" {
		t.Errorf("hub tags = %q", hub.Metadata["tags"])
	}
	want := "/plans/big/steps.md: the write policy refuses it without metadata source"
	if !slices.Contains(p.Unfixed, want) || len(p.Unfixed) != len(p.Writes) {
		t.Errorf("unfixed = %q, want %q for each write", p.Unfixed, want)
	}
}

func TestSplitRefusesWhatItCannotName(t *testing.T) {
	flat := "# Flat\n\nSummary here.\n\n" + strings.Repeat("line\n", 2000)
	p := Propose(&Input{Path: "/f.md", Body: flat})
	if p.Kind != KindReport || !strings.Contains(strings.Join(p.Unfixed, " "), "no second-level headings") || !strings.Contains(p.Report(nil, nil), "- no second-level headings") {
		t.Errorf("flat = %+v", p)
	}
	dup := "# Dup\n\nSummary here.\n\n## Same\n\n" + strings.Repeat("a\n", 3000) + "\n## Same\n\n" + strings.Repeat("b\n", 3000)
	if p := Propose(&Input{Path: "/d.md", Body: dup}); p.Kind != KindReport || !strings.Contains(strings.Join(p.Unfixed, " "), "slug to") {
		t.Errorf("dup = %+v", p)
	}
}

func TestSplitMovesShortSectionsWhenTheHubWouldStillOverflow(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Many\n\nSummary here.\n\n")
	for i := range 12 {
		b.WriteString("## S" + string(rune('a'+i)) + "\n\n" + strings.Repeat("word ", 180) + "\n\n")
	}
	p := Propose(&Input{Path: "/m.md", Body: b.String()})
	if p.Kind != KindSplit {
		t.Fatalf("kind %s: %v", p.Kind, p.Unfixed)
	}
	if hub := p.Writes[len(p.Writes)-1]; len(hub.Body) >= 8*1024 {
		t.Errorf("hub still %d bytes", len(hub.Body))
	}
}

func TestOneLineSkipsATable(t *testing.T) {
	if got := oneLine("## Records\n\n| Genre | ID |\n|---|:-:|\n| Adab | g-1 |\n", 7); got != "" {
		t.Errorf("table = %q, want none", got)
	}
	if got := oneLine("## Steps\n\nFirst sentence | with a pipe. Second.\n", 5); got != "First sentence | with a pipe." {
		t.Errorf("prose = %q", got)
	}
}

func TestReplaceEmDashes(t *testing.T) {
	in := "a — b\n— lead\n- — item\n`x — y` and z—w\n```\nc — d\n```\ne —  f\n~~~\n```\ng — h\n~~~\n``i — ` j`` k — l\n````\nm — n\n``` o\n````\n"
	got, n := replaceEmDashes(in)
	want := "a, b\n— lead\n- — item\n`x — y` and z, w\n```\nc — d\n```\ne, f\n~~~\n```\ng — h\n~~~\n``i — ` j`` k, l\n````\nm — n\n``` o\n````\n"
	if got != want || n != 4 {
		t.Errorf("n %d\n%s\nwant\n%s", n, got, want)
	}
}

func TestWithoutStatus(t *testing.T) {
	for in, want := range map[string][2]string{
		"Step 3 (DONE 2026-10-12)": {"Step 3", "DONE 2026-10-12"},
		"Rollout: SHIPPED":         {"Rollout", "SHIPPED"},
		"PR #12 review (draft)":    {"PR review (draft)", "#12"},
		"2026-10-12":               {"", "2026-10-12"},
	} {
		name, status := withoutStatus(in)
		if name != want[0] || status != want[1] {
			t.Errorf("withoutStatus(%q) = %q, %q; want %q, %q", in, name, status, want[0], want[1])
		}
	}
}

func TestInboundFrom(t *testing.T) {
	body := "See [a](/doc.md#one), [b](doc.md#two), [c](mark://host/doc.md#three), [d](/doc.md) and [e](/other.md#x).\n"
	got := InboundFrom(&Source{Name: "S", Path: "/src.md", Body: body}, "/doc.md", "mark://host:6309/doc.md")
	if len(got) != 3 || got[0].Anchor != "one" || got[1].Anchor != "two" || got[2].Anchor != "three" || got[0].Source != "S" {
		t.Errorf("inbound = %+v", got)
	}
}

func docsOn(backend *fetchtest.Client) func(string) *docwrite.Doc {
	return func(p string) *docwrite.Doc {
		return &docwrite.Doc{Backend: backend, Host: "h", Path: p, ReadToken: "r", Write: docwrite.SendOnce("w")}
	}
}

func TestApplyPreflightsTopicsAndWritesTheHubLast(t *testing.T) {
	in := &Input{Path: "/plans/big.md", Body: bigDoc(), Version: 2, Metadata: map[string]string{"tags": "plan"}}
	p := Propose(in)
	stamp := map[string]string{"agent": "a"}

	backend := &fetchtest.Client{Published: map[string]fetch.Result{"h/plans/big/design.md": fetchtest.Head("# Something else\n", 1, nil)}}
	applied, err := Apply(t.Context(), docsOn(backend), &p, stamp)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Path != "/plans/big/design.md" || len(applied) != 0 || len(backend.PublishCalls) != 0 {
		t.Fatalf("differing topic: %v %+v publishes %d", err, applied, len(backend.PublishCalls))
	}

	design := p.Writes[0]
	backend = &fetchtest.Client{Published: map[string]fetch.Result{
		"h/plans/big/design.md": fetchtest.Head(design.Body, 1, design.Metadata),
		"h/plans/big.md":        fetchtest.Head(in.Body, 2, in.Metadata),
	}}
	applied, err = Apply(t.Context(), docsOn(backend), &p, stamp)
	if err != nil || len(applied) != 4 || applied[0].Action != ActionSkipped || applied[1].Action != ActionCreated || applied[3].Action != ActionUpdated || applied[3].Version != 1 {
		t.Fatalf("apply: %v %+v", err, applied)
	}
	if len(backend.PublishCalls) != 3 || backend.PublishCalls[0].ExpectedVersion != 0 || backend.PublishCalls[2].Path != "/plans/big.md" || backend.PublishCalls[2].ExpectedVersion != 2 || backend.PublishCalls[2].Metadata["agent"] != "a" {
		t.Errorf("publishes = %+v", backend.PublishCalls)
	}

	// A topic an earlier apply of this document wrote is updated at any
	// version while its digest still matches; one edited since refuses.
	topicAt := func(body string, version int, digestOf string) *fetchtest.Client {
		meta := map[string]string{neardup.CuratedFromKey: "/plans/big.md", "tags": "x"}
		meta[curatedDigest] = topicDigest(digestOf, meta)
		return &fetchtest.Client{Published: map[string]fetch.Result{"h/plans/big/design.md": fetchtest.Head(body, version, meta), "h/plans/big.md": fetchtest.Head(in.Body, 2, in.Metadata)}}
	}
	older := "# Big plan: Design\n\nolder split\n"
	backend = topicAt(older, 3, older)
	applied, err = Apply(t.Context(), docsOn(backend), &p, stamp)
	if err != nil || applied[0].Action != ActionUpdated || backend.PublishCalls[0].ExpectedVersion != 3 {
		t.Fatalf("resumed split: %v %+v %+v", err, applied[0], backend.PublishCalls[0])
	}
	backend = topicAt("# Big plan: Design\n\nmy edit\n", 4, older)
	if _, err = Apply(t.Context(), docsOn(backend), &p, stamp); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "differs") || len(backend.PublishCalls) != 0 {
		t.Fatalf("edited topic: %v (publishes %d)", err, len(backend.PublishCalls))
	}

	backend = &fetchtest.Client{PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
		if r.Path == "/plans/big.md" {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict}}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": "1"}}}, nil
	}}
	applied, err = Apply(t.Context(), docsOn(backend), &p, stamp)
	if !errors.As(err, &refusal) || refusal.Reason != protocol.StatusConflict || len(applied) != 3 {
		t.Fatalf("conflict: %v %+v", err, applied)
	}
	if _, err := Apply(t.Context(), docsOn(backend), &Proposal{Kind: KindClean}, nil); err == nil || !strings.Contains(err.Error(), "nothing to apply") {
		t.Errorf("clean apply = %v", err)
	}
}
