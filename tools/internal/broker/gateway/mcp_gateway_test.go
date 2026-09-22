package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
)

func TestMCPGatewayInitializeHandshake(t *testing.T) {
	v := &brokertest.FakeVerifier{Claims: core.Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	ts := newTestMCPGateway(t, mcpTestConfig(), v)

	resp := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if resp.HTTPStatus != http.StatusOK {
		t.Fatalf("initialize: status = %d, body = %s", resp.HTTPStatus, resp.RawBody)
	}
	if resp.Headers[mcpSessionHeader] == "" {
		t.Errorf("initialize response missing %s header (clients need it for subsequent requests)", mcpSessionHeader)
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want \"2.0\"", resp.JSONRPC)
	}
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %+v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatalf("initialize: nil result")
	}
	caps, ok := resp.Result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result missing capabilities object: %+v", resp.Result)
	}
	if _, ok := caps["tools"]; !ok {
		t.Errorf("initialize capabilities missing tools advertisement: %+v", caps)
	}
	// Resources and prompts joined the surface in the resources+prompts
	// follow-up (this flip was the follow-up's declared starting move;
	// the original gateway plan had them Out-of-Scope). They must now
	// advertise so Desktop pickers and slash commands light up.
	if _, ok := caps["resources"]; !ok {
		t.Errorf("initialize capabilities missing resources advertisement: %+v", caps)
	}
	if _, ok := caps["prompts"]; !ok {
		t.Errorf("initialize capabilities missing prompts advertisement: %+v", caps)
	}
	info, ok := resp.Result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result missing serverInfo: %+v", resp.Result)
	}
	if name, _ := info["name"].(string); name != "demarkus-knowledge-broker" {
		t.Errorf("serverInfo.name = %q, want \"demarkus-knowledge-broker\"", name)
	}
}

func TestMCPGatewayToolsListMatchesAdvertisedNames(t *testing.T) {
	v := &brokertest.FakeVerifier{Claims: core.Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	ts := newTestMCPGateway(t, mcpTestConfig(), v)

	// Initialize first — Streamable HTTP requires the client to echo
	// the Mcp-Session-Id header (returned in the init response) on every
	// follow-up request, so tests do the handshake and thread the
	// session ID through.
	initR := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if initR.HTTPStatus != http.StatusOK {
		t.Fatalf("initialize: status = %d, body = %s", initR.HTTPStatus, initR.RawBody)
	}
	sessionID := initR.Headers[mcpSessionHeader]
	if sessionID == "" {
		t.Fatalf("initialize response missing %s header — cannot drive tools/list", mcpSessionHeader)
	}

	resp := mcpRequest(t, ts.URL, "alice-token", sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	if resp.HTTPStatus != http.StatusOK {
		t.Fatalf("tools/list: status = %d, body = %s", resp.HTTPStatus, resp.RawBody)
	}
	if resp.Error != nil {
		t.Fatalf("tools/list returned JSON-RPC error: %+v", resp.Error)
	}
	tools, ok := resp.Result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list: result.tools missing or wrong type: %+v", resp.Result)
	}
	if len(tools) != len(mcpToolNames) {
		t.Fatalf("tools/list returned %d tools, want %d", len(tools), len(mcpToolNames))
	}
	got := make(map[string]bool, len(tools))
	for i, raw := range tools {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tool[%d] not an object: %+v", i, raw)
		}
		name, _ := entry["name"].(string)
		if name == "" {
			t.Fatalf("tool[%d] has empty name: %+v", i, entry)
		}
		got[name] = true
		desc, _ := entry["description"].(string)
		if desc == "" {
			t.Errorf("tool %q: description is empty", name)
		}
	}
	for _, want := range mcpToolNames {
		if !got[want] {
			t.Errorf("tools/list missing expected tool %q", want)
		}
	}
}

// TestMCPGatewayEveryAdvertisedToolHasAHandler is the canary
// the placeholder test from Slices 1-4a evolved into. All 13
// tools are real handlers now (Slice 4b+5); this test makes the
// "no placeholder fall-throughs in production" invariant
// permanent by asserting toolHandlers() returns a real handler
// for every advertised tool name. A future tool definition added
// to mcpToolNames without a matching handlers map entry fails
// here, not at runtime.
func TestMCPGatewayEveryAdvertisedToolHasAHandler(t *testing.T) {
	gw := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})

	handlers := gw.toolHandlers()
	for _, name := range mcpToolNames {
		if _, ok := handlers[name]; !ok {
			t.Errorf("toolHandlers() missing entry for %q — would fall through to notImplementedHandler", name)
		}
	}
	// Reverse direction: catch any handler map entry whose name
	// doesn't appear in mcpToolNames — would mean we wired a
	// handler but never advertised the tool definition.
	for name := range handlers {
		if !slices.Contains(mcpToolNames, name) {
			t.Errorf("toolHandlers() has %q but no advertised tool definition by that name", name)
		}
	}
}

func TestMCPGatewayPerSubjectRateLimitTriggers429(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.RateLimit = core.RateLimitConfig{
		Tokens: core.RateLimitRouteConfig{PerMinute: 60, Burst: 1},
		Login:  core.RateLimitRouteConfig{PerMinute: 60, Burst: 1},
	}
	v := brokertest.TwoSubjectVerifier()
	ts := newTestMCPGateway(t, cfg, v)

	// Initialize on burst=1: that's the only freebie. The second
	// request hits the rate limit and the bucket says no.
	body, _ := json.Marshal(initializeRequest(1))
	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer alice-token")
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		got := resp.StatusCode
		_ = resp.Body.Close()
		if got != want {
			t.Errorf("attempt %d status = %d, want %d", i, got, want)
		}
		if got == http.StatusTooManyRequests {
			if h := resp.Header.Get("Retry-After"); h == "" {
				t.Errorf("attempt %d: 429 missing Retry-After header", i)
			}
		}
	}

	// Cross-subject isolation: bob still has a fresh bucket even after
	// alice burned hers. Pinned here so a future refactor that subtly
	// broadens the rate-limit key (e.g. including the route in the hash)
	// doesn't silently regress.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer bob-token")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("bob's first request status = %d, want 200 (cross-subject isolation)", resp.StatusCode)
	}
}
