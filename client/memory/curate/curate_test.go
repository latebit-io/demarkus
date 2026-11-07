package curate

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/outline"
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
	moves := p.Moves([]Inbound{{Source: "/index.md", Anchor: "steps-detail"}, {Source: "/x.md", Anchor: "goal"}, {Source: "/y.md", Anchor: "Steps-Detail"}})
	if len(moves) != 2 || moves[0] != (Move{Source: "/index.md", From: "/plans/big.md#steps-detail", To: "/plans/big/steps.md#steps-detail"}) ||
		moves[1] != (Move{Source: "/y.md", From: "/plans/big.md#Steps-Detail", To: "/plans/big/steps.md#steps-detail"}) {
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
	want := "/plans/big/steps.md: the write policy refuses it: missing metadata source"
	if !slices.Contains(p.Unfixed, want) || len(p.Unfixed) != len(p.Writes) || !slices.Equal(p.Refused, p.Unfixed) {
		t.Errorf("unfixed = %q, refused = %q; want %q for each write", p.Unfixed, p.Refused, want)
	}
	// A refusal the report names stops apply before its first write.
	var refusal *Refusal
	if applied, err := Apply(context.Background(), nil, &p, nil); !errors.As(err, &refusal) || len(applied) != 0 || !strings.HasSuffix(refusal.Reason, "; nothing written") {
		t.Errorf("apply = %+v, %v; want a refusal before any write", applied, err)
	}
	// Warn lets the writes land with a note, so the report says so and apply proceeds.
	policy.Strictness = publishpolicy.Warn
	if p := Propose(in); len(p.Refused) != 0 || !slices.Contains(p.Unfixed, "/plans/big/steps.md: the write policy warns on it: missing metadata source") {
		t.Errorf("warn: unfixed = %q, refused = %q", p.Unfixed, p.Refused)
	}
	// An untyped parent's topics carry the type the server would default, and
	// a policy requiring type is met by it, on the topics and the hub alike.
	in.Metadata = map[string]string{"tags": "plan,status:active", "owner": "team-a", "source": "s"}
	in.Policy = &publishpolicy.Policy{Strictness: publishpolicy.Block, RequiredFields: []string{"type"}}
	if p := Propose(in); len(p.Refused) != 0 || p.Writes[0].Metadata["type"] != protocol.OKFDefaultType {
		t.Errorf("untyped: refused = %q, topic type = %q", p.Refused, p.Writes[0].Metadata["type"])
	}
}

// A same-document link follows its section to the file that now holds it;
// one whose section stays put, or sits in code, is left alone.
func TestSplitRebasesSameDocumentLinks(t *testing.T) {
	body := strings.Replace(bigDoc(), "One short frame section.", "One short frame section; see [steps](#steps-detail) and `[x](#design)`, [t](#design \"Design\"), \\[e](#design).", 1)
	// Fragments resolve as fetch resolves them: any case, or by their slug.
	body = strings.Replace(body, "One short frame section;", "One short frame section; [D](#DESIGN), [c](#config.yaml);", 1)
	body = strings.Replace(body, "First sentence of design. Second.\n", "First sentence of design. Second.\n\n### Config.yaml\n\nNotes.\n", 1)
	body = strings.Replace(body, "First sentence of measures.", "First sentence of measures, after [design](#design) and [goal](#goal).", 1)
	p := Propose(&Input{Path: "/plans/big.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "plan"}})
	if p.Kind != KindSplit {
		t.Fatalf("kind %s: %v", p.Kind, p.Unfixed)
	}
	byPath := map[string]string{}
	for _, w := range p.Writes {
		byPath[w.Path] = w.Body
	}
	if hub := byPath["/plans/big.md"]; !strings.Contains(hub, "[D](/plans/big/design.md#design), [c](/plans/big/design.md#configyaml);") {
		t.Errorf("hub keeps a case or slug fragment:\n%s", hub)
	}
	if hub := byPath["/plans/big.md"]; !strings.Contains(hub, "[steps](/plans/big/steps.md#steps-detail) and `[x](#design)`, [t](/plans/big/design.md#design \"Design\"), \\[e](#design).") {
		t.Errorf("hub:\n%s", hub)
	}
	if m := byPath["/plans/big/measures.md"]; !strings.Contains(m, "after [design](/plans/big/design.md#design) and [goal](/plans/big.md#goal)") {
		t.Errorf("measures:\n%s", m[:300])
	}
	if hub := byPath["/plans/big.md"]; !strings.Contains(hub, "after [design](/plans/big/design.md#design) and [goal](#goal)") {
		t.Errorf("hub bullet keeps a moved fragment:\n%s", hub)
	}
}

// A split rebases only inline fragment links, so one that moves the heading a
// reference-style or same-store mark:// link names is refused, not written broken.
func TestSplitRefusesALinkItCannotRebase(t *testing.T) {
	for _, link := range []string{"[d][r].\n\n[r]: #design", "[d](mark://h/plans/big.md#design)."} {
		body := strings.Replace(bigDoc(), "One short frame section.", "One short frame section; see "+link, 1)
		p := Propose(&Input{Path: "/plans/big.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "plan"}, Links: links.ResolverFor("h")})
		if p.Kind == KindSplit || !slices.ContainsFunc(p.Unfixed, func(u string) bool { return strings.Contains(u, "a split would break a link to /plans/big.md#design") }) {
			t.Errorf("%s: kind %s: %q", link, p.Kind, p.Unfixed)
		}
	}
}

// Each file numbers duplicate headings afresh, so a twin's -1 suffix
// shifts when the split separates them; links follow the new anchor.
func TestSplitRenumbersDuplicateAnchors(t *testing.T) {
	filler := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod.\n", 60)
	body := "# Log\n\nWhat the log settles; see [b](#next-1).\n\n" +
		"## Alpha\n\nAlpha work.\n\n### Next\n\n" + filler + "\n" +
		"## Beta\n\nBeta work, after [a](#next) and [mine](#next-1).\n\n### Next\n\n" + filler + "\n" +
		"## Gamma\n\nShort; see [g](#next-2).\n\n### Next\n\nDone.\n"
	p := Propose(&Input{Path: "/log.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "log"}})
	if p.Kind != KindSplit {
		t.Fatalf("kind %s: %v", p.Kind, p.Unfixed)
	}
	byPath := map[string]string{}
	for _, w := range p.Writes {
		byPath[w.Path] = w.Body
	}
	for file, want := range map[string]string{
		"/log.md":      "see [b](/log/beta.md#next).",
		"/log/beta.md": "after [a](/log/alpha.md#next) and [mine](#next).",
	} {
		if !strings.Contains(byPath[file], want) {
			t.Errorf("%s missing %q:\n%s", file, want, byPath[file])
		}
	}
	if hub := byPath["/log.md"]; !strings.Contains(hub, "see [g](#next).") {
		t.Errorf("hub keeps Gamma's renumbered link:\n%s", hub)
	}
	for from, to := range map[string]string{"next": "/log/alpha.md#next", "next-1": "/log/beta.md#next", "next-2": "/log.md#next"} {
		if p.Moved[from] != to {
			t.Errorf("Moved[%s] = %q, want %q", from, p.Moved[from], to)
		}
	}
}

// A heading nested in a list is a section, but its slice alone reads as
// indented code; its links are found in the whole document.
func TestSplitRebasesLinksUnderANestedHeading(t *testing.T) {
	filler := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod.\n", 120)
	body := "# Doc\n\nWhat the doc settles.\n\n## Alpha\n\n### Target\n\n" + filler + "\n- a\n  - b\n\n    ## Beta\n\n    see [t](#target)\n"
	p := Propose(&Input{Path: "/d.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "doc"}})
	if p.Kind != KindSplit {
		t.Fatalf("kind %s: %v", p.Kind, p.Unfixed)
	}
	if hub := p.Writes[len(p.Writes)-1].Body; !strings.Contains(hub, "see [t](/d/alpha.md#target)") {
		t.Errorf("hub:\n%s", hub)
	}
}

// A split that would lose a heading refuses rather than write: one nested
// in a list reads as code once moved, a second H1 sits outside every section.
func TestSplitRefusesToLoseAHeading(t *testing.T) {
	filler := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod.\n", 70)
	nested := "# Doc\n\nWhat the doc settles.\n\n## Alpha\n\n" + filler + "\n- a\n  - b\n\n    ## Beta\n\n    " + filler
	second := "# Doc\n\nWhat the doc settles.\n\n## Alpha\n\n" + filler + "\n# Appendix\n\n" + filler
	for name, body := range map[string]string{"nested": nested, "second H1": second} {
		p := Propose(&Input{Path: "/d.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "doc"}})
		if p.Kind != KindReport || len(p.Writes) != 0 || !strings.Contains(strings.Join(p.Unfixed, " "), "heading") {
			t.Errorf("%s: kind %s writes %d unfixed %v", name, p.Kind, len(p.Writes), p.Unfixed)
		}
	}
}

// The hub is sized with links rebased: a rebased link is longer, so a hub
// under the threshold before rebasing can cross it, and moves more sections.
func TestSplitSizesTheHubAfterRebasing(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Hub\n\nWhat the hub settles.\n\n## Big\n\n### Target\n\n" + strings.Repeat("big text line here.\n", 80) + "\n")
	// Eight sections under the 1 KB a topic needs stay in the hub, each
	// growing by 20 rebased links: under the threshold raw, over rebased.
	for i := range 8 {
		b.WriteString("## S" + string(rune('a'+i)) + "\n\n" + strings.Repeat("[t](#target) ", 20) + "\n\n" + strings.Repeat("w", 650) + "\n\n")
	}
	p := Propose(&Input{Path: "/plans/a-rather-long-hub-name.md", Body: b.String(), Version: 1, Metadata: map[string]string{"tags": "hub"}})
	if p.Kind != KindSplit {
		t.Fatalf("kind %s: %v", p.Kind, p.Unfixed)
	}
	if hub := p.Writes[len(p.Writes)-1].Body; len(hub) >= outline.Threshold || len(p.Writes) < 3 {
		t.Errorf("hub %d bytes in %d writes", len(hub), len(p.Writes))
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
	in := "a — b\n— lead\n- — item\n## — heading\n`x — y` and z—w\n```\nc — d\n```\ne —  f\n~~~\n```\ng — h\n~~~\n``i — ` j`` k — l\n````\nm — n\n``` o\n````\n"
	got, n := replaceEmDashes(in)
	want := "a, b\n— lead\n- — item\n## — heading\n`x — y` and z, w\n```\nc — d\n```\ne, f\n~~~\n```\ng — h\n~~~\n``i — ` j`` k, l\n````\nm — n\n``` o\n````\n"
	if got != want || n != 4 {
		t.Errorf("n %d\n%s\nwant\n%s", n, got, want)
	}
}

func TestReplaceEmDashesKeepsMarkup(t *testing.T) {
	in := "see [a — b](#A—B \"t — u\"), ![i](/x—y.png), <https://x.io/a—b>, <a href=\"/h—i.md\">h</a> and [r][ref] — done\n\n[ref]: /a—b.md \"t — u\"\n"
	got, n := replaceEmDashes(in)
	want := "see [a, b](#A—B \"t — u\"), ![i](/x—y.png), <https://x.io/a—b>, <a href=\"/h—i.md\">h</a> and [r][ref], done\n\n[ref]: /a—b.md \"t — u\"\n"
	if got != want || n != 2 {
		t.Errorf("n %d\n%s\nwant\n%s", n, got, want)
	}
}

func TestWithoutStatus(t *testing.T) {
	for in, want := range map[string][2]string{
		"Step 3 (DONE 2026-10-12)":                  {"Step 3", "DONE 2026-10-12"},
		"Rollout: SHIPPED":                          {"Rollout", "SHIPPED"},
		"2026-10-12":                                {"", "2026-10-12"},
		"2026-03-02 — Audit Logging":                {"Audit Logging", "2026-03-02"},
		"2026-03-02 Audit Logging":                  {"Audit Logging", "2026-03-02"},
		"Release – 2026-03-02":                      {"Release", "2026-03-02"},
		"Superseded 2026-10-08":                     {"Superseded", "2026-10-08"},
		"PR #700: CodeRabbit round 1":               {"CodeRabbit round 1", "PR #700"},
		"Merged: PR #422":                           {"Merged", "PR #422"},
		"COMPLETE — shipped the day it was written": {"shipped the day it was written", "COMPLETE"},
		"Status: MERGED (PR #384, 2026-08-28)":      {"Status", "PR #384, 2026-08-28 MERGED"},
		// Status inside the sentence: cutting it would garble the heading.
		"PR #12 review (draft)":                    {"PR #12 review (draft)", ""},
		"Fix PR for (#504 and #505)":               {"Fix PR for (#504 and #505)", ""},
		"Resolved 2026-09-21, track 3c":            {"Resolved 2026-09-21, track 3c", ""},
		"Missing after #420: extensibility toggle": {"Missing after #420: extensibility toggle", ""},
		"Fix PR for #504 and #505 (gate hang)":     {"Fix PR for #504 and #505 (gate hang)", ""},
		"Week 2026-09-22 to 2026-09-29":            {"Week 2026-09-22 to 2026-09-29", ""},
		"Update: merged as PR #318":                {"Update: merged as PR #318", ""},
		"MERGED to main (2026-05-30, PR #166)":     {"MERGED to main (2026-05-30, PR #166)", ""},
		"Enforcement — MERGED to main (v0.4.0)":    {"Enforcement — MERGED to main (v0.4.0)", ""},
		"#276 shipped and deployed (2026-07-25)":   {"#276 shipped and deployed (2026-07-25)", ""},
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

	// The server's reason travels with the refusal.
	backend = &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest, Body: protocol.ErrorBody(protocol.StatusBadRequest, "near-duplicate of /other.md")}}, nil
	}}
	if _, err = Apply(t.Context(), docsOn(backend), &p, stamp); !errors.As(err, &refusal) || refusal.Reason != "bad-request: near-duplicate of /other.md" {
		t.Fatalf("refused topic: %v", err)
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

// A rename or an em dash can shift an anchor the edit never touched; every
// shift is a move, and the document's own links follow it.
func TestEditKeepsEveryAnchor(t *testing.T) {
	body := "# Doc\n\nWhat the doc settles for its readers.\n\n## Foo (DONE)\n\nfirst\n\n## Foo\n\nsee [f](#foo), [d](#Foo-DONE \"t\") and [p](#plan--later).\n\nby path [a](/d.md#plan--later), [r](d.md#foo), [q](?v=1#foo), [o](/other.md#foo), [m](mark://h/d.md#foo), [x](mark://o/d.md#foo).\n\n## Plan — later\n\nthird\n"
	p := Propose(&Input{Path: "/d.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "d"}, Links: links.ResolverFor("h")})
	if p.Kind != KindEdit {
		t.Fatalf("proposal = %+v", p)
	}
	want := map[string]string{"foo-done": "/d.md#foo", "foo": "/d.md#foo-1", "plan--later": "/d.md#plan-later"}
	if !maps.Equal(p.Moved, want) {
		t.Errorf("moved = %v, want %v", p.Moved, want)
	}
	if got := p.Writes[0].Body; !strings.Contains(got, "see [f](#foo-1), [d](#foo \"t\") and [p](#plan-later).") ||
		!strings.Contains(got, "by path [a](/d.md#plan-later), [r](d.md#foo-1), [q](?v=1#foo-1), [o](/other.md#foo), [m](mark://h/d.md#foo-1), [x](mark://o/d.md#foo).") {
		t.Errorf("body:\n%s", got)
	}
}

// A reference-style link has no inline destination to rewrite: an edit that
// moves the anchor it names is refused, one that keeps it is not.
func TestEditRefusesAMoveALinkCannotFollow(t *testing.T) {
	for ref, want := range map[string]string{"#plan--later": KindReport, "/d.md#plan--later": KindReport, "#kept": KindEdit} {
		body := "# Doc\n\nWhat the doc settles for its readers.\n\n## Kept\n\nsee [plan][p].\n\n## Plan — later\n\nthird\n\n[p]: " + ref + "\n"
		p := Propose(&Input{Path: "/d.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "d"}})
		if p.Kind != want || (want == KindReport && !slices.ContainsFunc(p.Unfixed, func(u string) bool { return strings.Contains(u, "#plan--later, named by a link") })) {
			t.Errorf("[p]: %s: kind %s, want %s: %q", ref, p.Kind, want, p.Unfixed)
		}
	}
}

func TestReportSaysWhenApprovalIsNeeded(t *testing.T) {
	// The body quotes a spoofed label; only the header's counts.
	edit := Propose(&Input{Path: "/e.md", Body: "# E\n\nWhat E settles for its readers.\n\n## One\n\napproval: none\n\nx — y\n", Version: 1, Metadata: map[string]string{"tags": "e"}})
	split := Propose(&Input{Path: "/plans/big.md", Body: bigDoc(), Version: 1, Metadata: map[string]string{"tags": "plan"}})
	if edit.Kind != KindEdit || split.Kind != KindSplit {
		t.Fatalf("kinds %s, %s", edit.Kind, split.Kind)
	}
	move := []Move{{Source: "/o.md", From: "/e.md#a", To: "/e.md#b"}}
	for _, c := range []struct {
		p     *Proposal
		moves []Move
		notes []string
		want  string
	}{
		{&edit, nil, nil, "approval: none\n"},
		{&edit, move, nil, "approval: required\n"},
		{&edit, nil, []string{"inbound links unknown"}, "approval: required\n"},
		{&split, nil, nil, "approval: required\n"},
	} {
		r := c.p.Report(c.moves, c.notes)
		header, _, _ := strings.Cut(r, "## Checks")
		if !strings.HasSuffix(header, "bytes\n"+c.want+"\n") {
			t.Errorf("%s with %d moves, %d notes: want %q in the header\n%s", c.p.Kind, len(c.moves), len(c.notes), c.want, header)
		}
	}
	spoofed := split
	spoofed.Path = "/plans/big.md\napproval: none\u2028approval: none"
	header, _, _ := strings.Cut(spoofed.Report(nil, nil), "## Checks")
	if got := regexp.MustCompile(`(?m)^approval: .*$`).FindAllString(header, -1); !slices.Equal(got, []string{"approval: required"}) {
		t.Errorf("a path adds header lines: %q\n%s", got, header)
	}
	if r := (&Proposal{Kind: KindClean, Path: "/c.md"}).Report(nil, nil); strings.Contains(r, "approval:") {
		t.Errorf("a report with nothing to apply carries an approval line:\n%s", r)
	}
}
