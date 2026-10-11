package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/protocol"
)

// TestHandleMarkDiscoverHappyPath drives the simplest discover:
// world serves its agent manifest, broker forwards verbatim.
// Pins that the broker rewrites the path to the well-known
// manifest location (the agent only supplies the world URL).
func TestHandleMarkDiscoverHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			path := r.Path
			if path != protocol.WellKnownManifestPath {
				t.Errorf("dispatcher saw path=%q, want %q (broker must rewrite to well-known)", path, protocol.WellKnownManifestPath)
			}
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":  "2",
					"modified": "2026-05-21T12:00:00Z",
				},
				Body: "# team-a agent manifest\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkDiscover(withAliceClaims(context.Background()), callToolReq("mark_discover", map[string]any{
		"url": "mark://team-a/",
	}))
	if err != nil {
		t.Fatalf("handleMarkDiscover: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	for _, want := range []string{"status: ok", "version: 2", "modified: 2026-05-21T12:00:00Z", "# team-a agent manifest"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkDiscoverMissingURL(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkDiscover(withAliceClaims(context.Background()), callToolReq("mark_discover", nil))
	if err != nil {
		t.Fatalf("handleMarkDiscover: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing url, want true")
	}
}

func TestHandleMarkDiscoverNotFoundForwardsVerbatim(t *testing.T) {
	// World has no manifest. The local demarkus-mcp surfaces the
	// not-found status verbatim in the formatted output rather
	// than turning it into a tool error; broker follows the same
	// proxy contract.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkDiscover(withAliceClaims(context.Background()), callToolReq("mark_discover", map[string]any{
		"url": "mark://team-a/",
	}))
	if err != nil {
		t.Fatalf("handleMarkDiscover: %v", err)
	}
	if res.IsError {
		t.Errorf("isError = true on missing manifest (proxy contract: forward verbatim): %s", toolResultText(t, res))
	}
	if text := toolResultText(t, res); !strings.Contains(text, "status: not-found") {
		t.Errorf("expected status: not-found in output, got:\n%s", text)
	}
}

// TestMCPGatewayMarkDiscoverEndToEnd drives mark_discover through the full
// Streamable HTTP transport: one end-to-end test pins the wire shape, the
// unit tests above cover dispatch.
func TestMCPGatewayMarkDiscoverEndToEnd(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			path := r.Path
			if path != protocol.WellKnownManifestPath {
				t.Errorf("dispatcher saw path=%q, want well-known manifest", path)
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1", "modified": "2026-05-21T14:00:00Z"},
				Body:     "manifest via gateway",
			}}, nil
		},
	}
	ts := newTestMCPGatewayWith(t, cfg, &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, d)

	initR := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if initR.HTTPStatus != http.StatusOK {
		t.Fatalf("initialize: status = %d", initR.HTTPStatus)
	}
	sessionID := initR.Headers[mcpSessionHeader]
	if sessionID == "" {
		t.Fatalf("initialize response missing %s header — session negotiation regressed", mcpSessionHeader)
	}

	resp := mcpRequest(t, ts.URL, "alice-token", sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "mark_discover",
			"arguments": map[string]any{"url": "mark://team-a/"},
		},
	})
	if resp.HTTPStatus != http.StatusOK {
		t.Fatalf("tools/call: status = %d, body = %s", resp.HTTPStatus, resp.RawBody)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call JSON-RPC error: %+v", resp.Error)
	}
	isErr, _ := resp.Result["isError"].(bool)
	if isErr {
		t.Fatalf("tools/call isError = true: %+v", resp.Result)
	}
	contents, _ := resp.Result["content"].([]any)
	if len(contents) == 0 {
		t.Fatalf("content empty")
	}
	first, _ := contents[0].(map[string]any)
	text, _ := first["text"].(string)
	for _, want := range []string{"status: ok", "version: 1", "manifest via gateway"} {
		if !strings.Contains(text, want) {
			t.Errorf("end-to-end text missing %q\nfull:\n%s", want, text)
		}
	}
}
