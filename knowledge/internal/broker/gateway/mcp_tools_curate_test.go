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

// A memory tenant curates and archives in its own world through the gateway;
// the cross-tenant invariant test keeps every other world out.
func TestMemoryCurateAndRecursiveArchiveOwnWorld(t *testing.T) {
	d := seededDispatcher()
	d.Published["alice-w/notes.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Notes\n\n## Done (SHIPPED)\n\nx — y\n", Metadata: map[string]string{"version": "2", "tags": "notes"}}}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	ctx := withAliceClaims(context.Background())

	res, err := g.tenantGate(g.toolHandlers()["mark_curate"])(ctx, callToolReq("mark_curate", map[string]any{"url": "mark://alice-w/notes.md"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := toolResultText(t, res)
	if res.IsError || !strings.Contains(text, "status: edit\n") || !strings.Contains(text, "proposal: ") || len(d.PublishCalls) != 0 {
		t.Fatalf("report = %s", text)
	}
	_, after, _ := strings.Cut(text, "proposal: ")
	hash, _, _ := strings.Cut(after, "\n")
	res, err = g.tenantGate(g.toolHandlers()["mark_curate"])(ctx, callToolReq("mark_curate", map[string]any{"url": "mark://alice-w/notes.md", "apply": true, "proposal": hash}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if text = toolResultText(t, res); res.IsError || !strings.HasPrefix(text, "status: applied\n- /notes.md: updated v1\n") {
		t.Fatalf("apply = %s", text)
	}
	if len(d.PublishCalls) != 1 || d.PublishCalls[0].Host != "alice-w" || d.PublishCalls[0].Metadata["user"] != "alice@example.com" {
		t.Errorf("publish = %+v", d.PublishCalls)
	}

	d.ListFn = func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
		return fetchtest.ListPage(r.Path, "", "a.md"), nil
	}
	res, err = g.tenantGate(g.toolHandlers()["mark_archive"])(ctx, callToolReq("mark_archive", map[string]any{"url": "mark://alice-w/old/", "recursive": true}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if text = toolResultText(t, res); res.IsError || !strings.HasPrefix(text, "status: archived\narchived: 1 of 1\n- /old/a.md: v3\n") {
		t.Fatalf("archive = %s", text)
	}
	if len(d.ArchiveCalls) != 1 || d.ArchiveCalls[0].Host != "alice-w" {
		t.Errorf("archives = %+v", d.ArchiveCalls)
	}
	assertNoWorldTraffic(t, d, "bob-w")
}
