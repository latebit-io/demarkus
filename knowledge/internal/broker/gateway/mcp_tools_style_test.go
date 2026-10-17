package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/protocol"
)

// A memory tenant's publish is held to the style rules in the write path:
// the write lands and the response names what to fix (ADR 0040).
func TestMemoryPublishCarriesStyleFindings(t *testing.T) {
	d := seededDispatcher()
	d.PublishFn = func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "1"}}}, nil
	}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_publish"])
	res, err := h(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://alice-w/notes.md",
		"body":             "No heading — and an em dash.\n",
		"expected_version": float64(0),
		"metadata":         map[string]any{"tags": "notes"},
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := toolResultText(t, res)
	if res.IsError {
		t.Fatalf("publish refused: %s", text)
	}
	for _, want := range []string{"status: ok", "\nnote: style problems in this publish: ", gate.Baseline} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q:\n%s", want, text)
		}
	}
	// The write lands, then the world's hub is linked to it.
	if len(d.PublishCalls) != 2 || d.PublishCalls[0].Path != "/notes.md" || d.PublishCalls[1].Path != "/index.md" {
		t.Errorf("publishes = %+v, want the write to land and the hub linked", d.PublishCalls)
	}
}

// The knowledge broker cites the root style guide instead.
func TestKnowledgePublishCitesRootStyleGuide(t *testing.T) {
	d := &fakeDispatcher{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "1"}}}, nil
	}}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url": "mark://team-a/foo.md", "body": "No heading.\n", "expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if text := toolResultText(t, res); res.IsError || !strings.Contains(text, gate.KnowledgeGuide) {
		t.Fatalf("response = %s", text)
	}
}
