package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
)

// A memory tenant audits its own world through the gateway; the tenant gate
// keeps every other world out (the cross-tenant invariant test covers that).
func TestMemoryDoctorAuditsOwnWorld(t *testing.T) {
	d := seededDispatcher()
	d.ListFn = func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
		if r.Host != "alice-w" {
			t.Errorf("list reached world %q", r.Host)
		}
		return fetch.Result{Response: render.ListResponse("/", []render.ListEntry{{Name: "index.md"}, {Name: "loose.md"}}, "")}, nil
	}
	d.Published["alice-w/loose.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Loose\n\nNothing links here.\n", Metadata: map[string]string{"tags": "x", "type": "Guide"}}}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_doctor"])
	res, err := h(withAliceClaims(context.Background()), callToolReq("mark_doctor", map[string]any{"url": "mark://alice-w/"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := toolResultText(t, res)
	if res.IsError {
		t.Fatalf("doctor refused: %s", text)
	}
	for _, want := range []string{"## / hygiene report  (2 docs)", "### Orphans (1)\n- /loose.md:"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	assertNoWorldTraffic(t, d, "bob-w")
}
