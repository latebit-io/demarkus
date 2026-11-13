package driftreport

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/memory/audit"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/dispatch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/server/knowledgeserver/knowledgetest"
)

const world = "team"

// replica is a real knowledge server serving one world, reached in process
// as a broker pod reaches it.
func replica(t *testing.T) *dispatch.Composite {
	t.Helper()
	registry := core.NewWorldRegistry([]core.WorldConfig{{Name: world, Namespace: world, Profile: core.ProfileKnowledge, Local: true}})
	all := registry.All()
	server := knowledgetest.Open(t, &knowledgetest.Buckets{}, knowledgetest.World{Name: all[0].Name, Authority: all[0].Authority()})
	if err := dispatch.CheckLocal(registry, server); err != nil {
		t.Fatal(err)
	}
	return dispatch.NewComposite(registry, server, nil)
}

// publish writes body as docPath's next version under user's verified identity.
func publish(t *testing.T, store *dispatch.Composite, user, docPath, body string, meta map[string]string) {
	t.Helper()
	ctx := protocol.WithGrant(context.Background(), protocol.Grant{Label: "test", User: user, Paths: []string{"/**"}})
	res, err := store.Publish(ctx, fetch.WriteRequest{Host: world, Path: docPath, Body: body, ExpectedVersion: -1, Metadata: meta})
	if err != nil || !protocol.IsWriteSuccess(res.Response.Status) {
		t.Fatalf("publish %s: %q %v", docPath, res.Response.Status, err)
	}
}

func fetchReport(t *testing.T, store *dispatch.Composite) fetch.Result {
	t.Helper()
	res, err := store.Fetch(context.Background(), fetch.FetchRequest{Host: world, Path: protocol.DriftReportPath})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// One real world end to end: the report lands under the server's own write
// gates, an unchanged world writes nothing, and a moved source writes again.
func TestReportFollowsTheWorld(t *testing.T) {
	t.Parallel()
	ctx, store := context.Background(), replica(t)
	r := newReporter(Config{Worlds: []string{world}, Store: store})
	tags := map[string]string{"tags": "adr"}
	publish(t, store, "alice", "/index.md", "# Team\n\nWhat the team keeps.\n\n- [A](/adr/a.md)\n- [R](/notes/r.md)\n", map[string]string{"tags": "hub"})
	publish(t, store, "alice", "/adr/a.md", "# A\n\nWhat A settles.\n", tags)
	publish(t, store, "alice", "/notes/r.md", "# R\n\nWhat R says of A.\n", map[string]string{"tags": "notes", provenance.Key: "/adr/a.md/v1"})

	if outcome, err := r.report(ctx, world); err != nil || outcome != "published, 0 documents drifted" {
		t.Fatalf("first report = %q, %v", outcome, err)
	}
	first := fetchReport(t, store)
	if !strings.Contains(first.Response.Body, "- pinned: 1 of 2 documents (50%)\n") || !strings.Contains(first.Response.Body, "\nNothing has drifted.\n") {
		t.Errorf("first report:\n%s", first.Response.Body)
	}
	if m := first.Response.Metadata; m[protocol.MetaAgent] != "demarkus-drift" || m["type"] != "Reference" || m[protocol.MetaUser] != "" {
		t.Errorf("report metadata = %v", m)
	}
	// The report is no document of the audit, so a second pass sees the same world.
	if outcome, err := r.report(ctx, world); err != nil || outcome != "unchanged" {
		t.Errorf("second report = %q, %v", outcome, err)
	}

	publish(t, store, "bob", "/adr/a.md", "# A\n\nWhat A settles now.\n", tags)
	if outcome, err := r.report(ctx, world); err != nil || outcome != "published, 1 documents drifted" {
		t.Fatalf("report after the source moved = %q, %v", outcome, err)
	}
	moved := fetchReport(t, store)
	want := "\n## Owner: alice\n\n- `/notes/r.md`: based on /adr/a.md, which is at v2, past the pinned v1\n"
	if !strings.Contains(moved.Response.Body, want) || moved.Response.Metadata["version"] != "2" {
		t.Errorf("report v%s lacks %q:\n%s", moved.Response.Metadata["version"], want, moved.Response.Body)
	}
}

// A hand write outside the report's path is refused under its grant.
func TestGrantReachesOnlyTheReport(t *testing.T) {
	t.Parallel()
	store := replica(t)
	res, err := store.Publish(protocol.WithGrant(context.Background(), Grant), fetch.WriteRequest{Host: world, Path: "/notes/x.md", Body: "# X\n\nNot a report.\n"})
	if err != nil || protocol.IsWriteSuccess(res.Response.Status) {
		t.Errorf("publish outside the report path = %q, %v; want a refusal", res.Response.Status, err)
	}
}

// Owners sort, the unowned come last, and a partial audit says so.
func TestRenderGroupsByOwner(t *testing.T) {
	got := Render(&audit.Report{
		Freshness: audit.Freshness{Eligible: 4, Pinned: 2},
		Coverage:  []string{"fetch /x.md failed: timeout"},
		Findings: []audit.Finding{
			{Check: audit.CheckDrift, Path: "/c.md", Detail: "based on /s.md, which is at v2, past the pinned v1"},
			{Check: audit.CheckDrift, Path: "/b.md", Detail: "based on /s.md, which is at v2, past the pinned v1", Owner: "zoe"},
			{Check: audit.CheckOrphans, Path: "/o.md", Detail: "no inbound link"},
			{Check: audit.CheckDrift, Path: "/a.md", Detail: "based on /gone.md, which is not a live document here", Owner: "amir"},
		},
	})
	want := "# Drift report\n\n" + summary + "\n\n## Freshness\n\n- pinned: 2 of 4 documents (50%)\n" +
		"\n## Owner: amir\n\n- `/a.md`: based on /gone.md, which is not a live document here\n" +
		"\n## Owner: zoe\n\n- `/b.md`: based on /s.md, which is at v2, past the pinned v1\n" +
		"\n## No owner\n\n- `/c.md`: based on /s.md, which is at v2, past the pinned v1\n" +
		"\n## Coverage\n\nThe audit was partial:\n\n- fetch /x.md failed: timeout\n"
	if got != want {
		t.Errorf("render:\n%s\nwant:\n%s", got, want)
	}
}
