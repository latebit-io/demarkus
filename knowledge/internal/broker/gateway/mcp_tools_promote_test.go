package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/protocol"
)

func promotePacket(t *testing.T, text string) string {
	t.Helper()
	_, after, _ := strings.Cut(text, "packet: ")
	hash, _, _ := strings.Cut(after, "\n")
	if hash == "" {
		t.Fatalf("no packet in:\n%s", text)
	}
	return hash
}

// A memory tenant stages and stamps in its own world; the knowledge phases
// are not served there. The cross-tenant invariant test keeps bob out.
func TestMemoryPromoteServesTheSoulPhasesOnly(t *testing.T) {
	d := seededDispatcher()
	d.Published["alice-w/adr/0001-x.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# X\n\nWhat x decides.\n", Metadata: map[string]string{"version": "2", "tags": "x", "type": "Decision"}}}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	ctx := withAliceClaims(context.Background())
	handler := g.tenantGate(g.toolHandlers()["mark_promote"])

	res, err := handler(ctx, callToolReq("mark_promote", map[string]any{"url": "mark://alice-w/adr/0001-x.md", "phase": "dry_run", "body": "# X\n"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if text := toolResultText(t, res); !res.IsError || !strings.Contains(text, `phase "dry_run" is not served here (stage, stamp, refresh)`) {
		t.Fatalf("dry_run on memory = %s", text)
	}
	res, err = handler(ctx, callToolReq("mark_promote", map[string]any{"url": "mark://alice-w/adr/0001-x.md", "phase": "stage"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := toolResultText(t, res)
	if res.IsError || !strings.Contains(text, "status: staged\n") || !strings.Contains(text, "candidate: decision record, type Decision\n") {
		t.Fatalf("stage = %s", text)
	}
	packet := promotePacket(t, text)
	res, err = handler(ctx, callToolReq("mark_promote", map[string]any{"url": "mark://alice-w/adr/0001-x.md", "phase": "stamp", "target": "mark://team-a/x.md@v1", "via": "knowledge", "packet": packet}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if text = toolResultText(t, res); res.IsError || text != "status: stamped\npath: /adr/0001-x.md\nversion: 1\nmode: full\n" {
		t.Fatalf("stamp = %s", text)
	}
	if len(d.PublishCalls) != 1 || d.PublishCalls[0].Host != "alice-w" || d.PublishCalls[0].Metadata["user"] != "alice@example.com" || d.PublishCalls[0].Metadata["tags"] != "x,promoted" {
		t.Errorf("publish = %+v", d.PublishCalls)
	}
	assertNoWorldTraffic(t, d, "bob-w")
}

// The knowledge broker checks a draft against the world's policy and
// publishes it under the caller's write grant, with provenance.
func TestKnowledgePromoteDryRunThenApply(t *testing.T) {
	d := &fakeDispatcher{Published: map[string]fetch.Result{
		"team-a/.well-known/demarkus/policy.md": fetchtest.Head("# Policy\n\nrequire_tags: domain\n", 1, nil),
		"team-a/index.md":                       fetchtest.Head("# Team A\n\n- [Docs](/docs/other.md)\n", 2, nil),
	}}
	d.LookupFn = func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
		return fetchtest.Lookup(r.Query, r.Scope, r.Match), nil
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	ctx := withAliceClaims(context.Background())
	handler := g.toolHandlers()["mark_promote"]
	draft := map[string]any{"url": "mark://team-a/docs/x.md", "phase": "dry_run", "body": "# X\n\nWhat x settles for the team.\n", "metadata": map[string]any{"tags": "x,domain:eng", "importance": "0.7"}, "source": "alice's soul"}

	res, err := handler(ctx, callToolReq("mark_promote", draft))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := toolResultText(t, res)
	if res.IsError || !strings.HasPrefix(text, "target: mark://team-a/docs/x.md\nstatus: ready\nversion: 0\npolicy: strictness warn; require_tags domain\n") {
		t.Fatalf("dry run = %s", text)
	}
	draft["phase"], draft["packet"] = "apply", promotePacket(t, text)
	res, err = handler(ctx, callToolReq("mark_promote", draft))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	// The world's hub names the promoted document, as it would a mark_publish.
	if text = toolResultText(t, res); res.IsError || text != "status: published\ntarget: mark://team-a/docs/x.md\nversion: 1\nhubs:\n- /index.md: bullet added\n" {
		t.Fatalf("apply = %s", text)
	}
	if hub := d.PublishCalls[1]; hub.Host != "team-a" || hub.Path != "/index.md" || !strings.HasSuffix(hub.Body, "- [X](/docs/x.md): What x settles for the team.\n") {
		t.Errorf("hub publish = %+v", hub)
	}
	w := d.PublishCalls[0]
	if w.Host != "team-a" || w.ExpectedVersion != 0 || w.Metadata["promoted-from"] != "alice's soul" || w.Metadata["user"] != "alice@example.com" || w.Metadata["tags"] != "x,domain:eng" {
		t.Errorf("publish = %+v", w)
	}
	draft["phase"] = "stage"
	res, err = handler(ctx, callToolReq("mark_promote", draft))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if text = toolResultText(t, res); !res.IsError || !strings.Contains(text, "not served here (dry_run, apply)") {
		t.Fatalf("stage on knowledge = %s", text)
	}
}
