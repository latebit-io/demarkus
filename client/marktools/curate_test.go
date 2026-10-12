package marktools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

const curateDoc = "# Doc\n\n## Step 3 (DONE 2026-10-12)\n\nShipped — finally.\n"

// proposalHash is the hash a report carries; a report without one fails.
func proposalHash(t *testing.T, report marktools.Result) string {
	t.Helper()
	_, after, _ := strings.Cut(report.Text, "proposal: ")
	hash, _, _ := strings.Cut(after, "\n")
	if report.IsError || hash == "" {
		t.Fatalf("no proposal hash in:\n%s", report.Text)
	}
	return hash
}

// The mechanic is tested in client/curate; here that the tool reads the head,
// finds the anchored links through the graph store, and words the report.
func TestCurateReportsWithInboundLinks(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/doc.md":   fetchtest.Head(curateDoc, 4, map[string]string{"tags": "a", "description": "What the doc settles for its readers today."}),
		"host:6309/other.md": fetchtest.Head("# Other\n\nSee [step](/doc.md#step-3-done-2026-10-12) and [all](doc.md).\n", 1, nil),
	}}
	surface := &clientSurface{DefaultHost: "mark://host:6309", Token: "tok", Store: graphstore.New()}
	tools := surface.tools(t, backend)
	surface.Store.ObserveDocument("mark://host:6309/other.md", graph.FetchResult{Status: protocol.StatusOK, Body: "[s](/doc.md#step-3-done-2026-10-12)"})
	got := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md"})
	if got.IsError {
		t.Fatalf("curate = %+v", got)
	}
	for _, want := range []string{"status: edit\npath: /doc.md\nversion: 4\n", "## Proposal: edit\n", "- mark://host/other.md: /doc.md#step-3-done-2026-10-12 -> /doc.md#step-3\n", "proposal: ", "=== /doc.md (description: "} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("report missing %q:\n%s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "note:") || len(backend.PublishCalls) != 0 {
		t.Errorf("report wrote or warned:\n%s", got.Text)
	}
	// Without a graph store the inbound list is a stated gap, not an empty claim.
	plain := newTools(t, backend, directHooks()).Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md"})
	if plain.IsError || !strings.Contains(plain.Text, "note: inbound links unknown: this server has no graph store") {
		t.Errorf("no store = %+v", plain)
	}
}

func TestCurateApplyBindsToTheProposal(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/doc.md": fetchtest.Head(curateDoc, 4, map[string]string{"tags": "a", "agent": "old"}),
	}}
	tools := newTools(t, backend, writingHooks())
	hash := proposalHash(t, tools.Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md"}))
	if got := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md", Apply: true}); !got.IsError || !strings.Contains(got.Text, "apply needs the proposal hash") {
		t.Errorf("apply without hash = %+v", got)
	}
	if got := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md", Apply: true, Proposal: "stale"}); !got.IsError || !strings.Contains(got.Text, "does not match the document at v4") {
		t.Errorf("stale hash = %+v", got)
	}
	if got := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/locked.md", Apply: true, Proposal: hash}); !got.IsError || got.Text != "publish requires a token" {
		t.Errorf("unauthorized apply = %+v", got)
	}
	if len(backend.PublishCalls) != 0 {
		t.Fatal("refusals wrote")
	}
	got := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md", Apply: true, Proposal: hash})
	if got.IsError || got.Text != "status: applied\n- /doc.md: updated v1\n" {
		t.Fatalf("apply = %+v", got)
	}
	w := backend.PublishCalls[0]
	if w.ExpectedVersion != 4 || w.Token != "write-token" || w.Metadata["tags"] != "a" || w.Metadata["agent"] != "test-agent" || !strings.Contains(w.Body, "## Step 3\n\nStatus: DONE 2026-10-12\n") {
		t.Errorf("publish = %+v", w)
	}
	// Nothing mechanical is left on the applied document; a second apply has nothing to do.
	again := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md", Apply: true, Proposal: hash})
	if !again.IsError || !strings.Contains(again.Text, "nothing to apply") {
		t.Errorf("reapply = %+v\n%s", again, tools.Curate(t.Context(), marktools.CurateArgs{URL: "/doc.md"}).Text)
	}
}

func bigBody() string {
	var b strings.Builder
	b.WriteString("# Big\n\nWhat big settles.\n\n")
	for _, name := range []string{"Alpha", "Beta"} {
		b.WriteString("## " + name + "\n\n" + strings.Repeat("Words about "+strings.ToLower(name)+". ", 400) + "\n\n")
	}
	return b.String()
}

func TestCurateApplySplitPreflightsTopics(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/plans/big.md": fetchtest.Head(bigBody(), 2, map[string]string{"tags": "plan", "type": "Plan"}),
	}}
	tools := newTools(t, backend, writingHooks())
	report := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/plans/big.md"})
	if !strings.Contains(report.Text, "status: split\n") {
		t.Fatalf("report = %+v", report)
	}
	hash := proposalHash(t, report)

	// A topic that exists and differs stops everything before the first write.
	backend.Published["host:6309/plans/big/alpha.md"] = fetchtest.Head("# Something else\n", 1, nil)
	got := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/plans/big.md", Apply: true, Proposal: hash})
	if !got.IsError || !strings.Contains(got.Text, "apply refused: /plans/big/alpha.md: exists and differs") || len(backend.PublishCalls) != 0 {
		t.Fatalf("differing topic = %+v, publishes %d", got, len(backend.PublishCalls))
	}
	// An identical topic (a resumed split) is skipped; the rest lands, the hub last at the reported version.
	_, alpha, _ := strings.Cut(report.Text, "=== /plans/big/alpha.md (")
	_, alphaBody, _ := strings.Cut(alpha, ")\n")
	alphaBody, _, _ = strings.Cut(alphaBody, "\n=== ")
	backend.Published["host:6309/plans/big/alpha.md"] = fetchtest.Head(alphaBody, 1, map[string]string{"tags": "big,alpha", "importance": "0.7", "type": "Plan", "curated-from": "/plans/big.md"})
	got = tools.Curate(t.Context(), marktools.CurateArgs{URL: "/plans/big.md", Apply: true, Proposal: hash})
	if got.IsError || got.Text != "status: applied\n- /plans/big/alpha.md: skipped, identical\n- /plans/big/beta.md: created v1\n- /plans/big.md: updated v1\n" {
		t.Fatalf("apply = %+v", got)
	}
	if len(backend.PublishCalls) != 2 || backend.PublishCalls[0].Path != "/plans/big/beta.md" || backend.PublishCalls[0].ExpectedVersion != 0 || backend.PublishCalls[1].ExpectedVersion != 2 {
		t.Errorf("publishes = %+v", backend.PublishCalls)
	}
}

func TestCurateApplyStopsOnAConflictAndSaysWhatLanded(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"host:6309/plans/big.md": fetchtest.Head(bigBody(), 2, map[string]string{"tags": "plan"})},
	}
	tools := newTools(t, backend, writingHooks())
	hash := proposalHash(t, tools.Curate(t.Context(), marktools.CurateArgs{URL: "/plans/big.md"}))
	backend.PublishFn = func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
		if r.Path == "/plans/big.md" {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict}}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": "1"}}}, nil
	}
	got := tools.Curate(t.Context(), marktools.CurateArgs{URL: "/plans/big.md", Apply: true, Proposal: hash})
	if !got.IsError || !strings.HasPrefix(got.Text, "status: partial\n- /plans/big/alpha.md: created v1\n- /plans/big/beta.md: created v1\n- /plans/big.md: conflict\n") {
		t.Fatalf("apply = %+v", got)
	}
}
