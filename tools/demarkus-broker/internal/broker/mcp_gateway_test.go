package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// mcpTestConfig returns a Config like testConfig() but with the MCP
// gateway configured. Setting MCP.Addr is the on-switch. The two
// PublicURLs are deliberately distinct (split-host topology) so tests
// catch any handler building a URL from the wrong one.
func mcpTestConfig() *Config {
	cfg := testConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	cfg.Server.MCP = MCPConfig{Addr: ":0", PublicURL: "https://gateway.example.com"}
	return cfg
}

// newTestMCPGateway builds a Server and returns an httptest.Server
// hosting the gateway's Routes (NOT the management API). Mirrors the
// newTestServer / newTestServerWithSigner shape so a single helper
// covers the integration tests in this file plus the focused tests
// in mcp_auth_test.go / mcp_oauth_test.go.
func newTestMCPGateway(t *testing.T, cfg *Config, verifier Verifier) *httptest.Server {
	t.Helper()
	signer := newTestSigner(t)
	k8s := fake.NewSimpleClientset()
	brokerSrv := NewServer(cfg, signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	ts := httptest.NewServer(brokerSrv.MCPGateway("test"))
	t.Cleanup(ts.Close)
	return ts
}

// mcpRequest fires a JSON-RPC 2.0 request at the gateway's /mcp
// endpoint and returns the parsed response (or the raw HTTP status +
// body when the response is non-200). bearer is the value of the
// Authorization header — pass "" to omit it (for unauth tests).
type mcpResponse struct {
	HTTPStatus int
	RawBody    []byte
	JSONRPC    string                     `json:"jsonrpc"`
	ID         json.Number                `json:"id"`
	Result     map[string]any             `json:"result,omitempty"`
	Error      map[string]any             `json:"error,omitempty"`
	Headers    map[string]string          `json:"-"`
	Extras     map[string]json.RawMessage `json:",omitempty"`
}

// mcpSessionHeader is the HTTP header MCP's Streamable HTTP transport
// uses to carry the session ID across requests. The server returns it
// in the initialize response; clients echo it on every subsequent
// non-initialize call. Hard-coded literal here instead of importing
// mcpserver.HeaderKeySessionID to keep this test file's imports lean.
const mcpSessionHeader = "Mcp-Session-Id"

func mcpRequest(t *testing.T, base, bearer, sessionID string, body map[string]any) mcpResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if sessionID != "" {
		req.Header.Set(mcpSessionHeader, sessionID)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rawBody, _ := io.ReadAll(resp.Body)
	out := mcpResponse{
		HTTPStatus: resp.StatusCode,
		RawBody:    rawBody,
		Headers:    map[string]string{},
	}
	for k, v := range resp.Header {
		if len(v) > 0 {
			out.Headers[k] = v[0]
		}
	}
	if resp.StatusCode == http.StatusOK && len(rawBody) > 0 {
		if err := json.Unmarshal(rawBody, &out); err != nil {
			t.Fatalf("decode JSON-RPC response: %v\nbody: %s", err, rawBody)
		}
	}
	return out
}

// initializeRequest is the standard MCP initialize payload. Kept as a
// helper so every test that needs an initialize handshake builds the
// same shape (and a future protocol-version bump is one edit).
func initializeRequest(id int) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "broker-mcp-test",
				"version": "test",
			},
		},
	}
}

func TestMCPGatewayInitializeHandshake(t *testing.T) {
	v := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
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
	if name, _ := info["name"].(string); name != "demarkus-broker" {
		t.Errorf("serverInfo.name = %q, want \"demarkus-broker\"", name)
	}
}

func TestMCPGatewayToolsListMatchesAdvertisedNames(t *testing.T) {
	v := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
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
	signer := newTestSigner(t)
	verifier := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	k8s := fake.NewSimpleClientset()
	srv := NewServer(mcpTestConfig(), signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	gw := newMCPGateway(srv, "test", &fakeDispatcher{})

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
	cfg.RateLimit = RateLimitConfig{
		Tokens: RateLimitRouteConfig{PerMinute: 60, Burst: 1},
		Login:  RateLimitRouteConfig{PerMinute: 60, Burst: 1},
	}
	v := twoSubjectVerifier()
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
