package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/latebit-io/demarkus/tools/internal/broker/storage"
	"github.com/mark3labs/mcp-go/mcp"
	"k8s.io/client-go/kubernetes/fake"
)

// fixedTestTime is the instant every gateway fixture clock starts at.
var fixedTestTime = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

// gatewayFixture is one SharedDeps wired the way Run wires the gateway, with
// a fake Secret store and a clock that starts at fixedTestTime.
type gatewayFixture struct {
	cfg         *core.Config
	shared      core.SharedDeps
	store       core.SecretStore
	clock       *brokertest.FakeClock
	profile     *Profile
	provisioner *storage.Provisioner
}

func newGatewayFixture(t testing.TB, cfg *core.Config, verifier core.Verifier, profile *Profile) *gatewayFixture {
	t.Helper()
	return gatewayFixtureFor(t, cfg, core.SharedDeps{Verifier: verifier}, profile)
}

// gatewayFixtureFor builds the fixture around shared, filling the store, the
// clock, the log and the subject limiter.
func gatewayFixtureFor(t testing.TB, cfg *core.Config, shared core.SharedDeps, profile *Profile) *gatewayFixture {
	t.Helper()
	store := storage.NewK8sSecretStore(fake.NewSimpleClientset())
	clock := brokertest.NewFakeClock(fixedTestTime)
	shared.Log, shared.Clock = slog.Default(), clock.Now
	shared.SubjectLimiter, _ = core.NewRateLimits(&cfg.RateLimit)
	return &gatewayFixture{cfg: cfg, shared: shared, store: store, clock: clock, profile: profile}
}

// enableProvisioning wires dynamic tenant provisioning over the fixture's
// store and clock.
func (f *gatewayFixture) enableProvisioning(buckets storage.BucketCreator) *storage.Provisioner {
	f.provisioner = storage.NewProvisioner(f.cfg, storage.ProvisionerDeps{Store: f.store, Buckets: buckets, Log: f.shared.Log, Clock: f.clock.Now})
	return f.provisioner
}

// gateway builds the gateway around d, for tests that call handlers directly.
func (f *gatewayFixture) gateway(d WorldDispatcher) *Gateway {
	return New(DepsFor(f.cfg, f.shared, f.store, f.provisioner), "test", d, f.profile)
}

// serve hosts the gateway's routes, for tests that drive the HTTP transport.
func (f *gatewayFixture) serve(t testing.TB, d WorldDispatcher) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(f.gateway(d).Routes())
	t.Cleanup(ts.Close)
	return ts
}

// newGatewayWithDispatcher is a knowledge gateway run as alice, in process.
func newGatewayWithDispatcher(t testing.TB, cfg *core.Config, d WorldDispatcher) *Gateway {
	t.Helper()
	return newGatewayFixture(t, cfg, &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, KnowledgeProfile()).gateway(d)
}

// newMemoryGateway is a memory gateway run as alice, in process.
func newMemoryGateway(t testing.TB, cfg *core.Config, d WorldDispatcher) *Gateway {
	t.Helper()
	return newGatewayFixture(t, cfg, &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, MemoryProfile()).gateway(d)
}

// memoryGatewayWithProvisioning is newMemoryGateway with dynamic provisioning
// over the fake Secret store.
func memoryGatewayWithProvisioning(t *testing.T, cfg *core.Config, d WorldDispatcher) (*Gateway, *brokertest.FakeBuckets) {
	t.Helper()
	f := newGatewayFixture(t, cfg, &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, MemoryProfile())
	buckets := &brokertest.FakeBuckets{}
	f.enableProvisioning(buckets)
	return f.gateway(d), buckets
}

// newTestMCPGateway serves a knowledge gateway over HTTP with the production
// world pool, for tests that never reach a world.
func newTestMCPGateway(t *testing.T, cfg *core.Config, verifier core.Verifier) *httptest.Server {
	t.Helper()
	pool := NewWorldPool(cfg.Registry(), fetch.Options{})
	t.Cleanup(pool.Close)
	return newTestMCPGatewayWith(t, cfg, verifier, pool)
}

// newTestMCPGatewayWith serves a knowledge gateway over HTTP around d.
func newTestMCPGatewayWith(t *testing.T, cfg *core.Config, verifier core.Verifier, d WorldDispatcher) *httptest.Server {
	t.Helper()
	return newGatewayFixture(t, cfg, verifier, KnowledgeProfile()).serve(t, d)
}

// fakeDispatcher is the shared scriptable client; see client/fetchtest.
type fakeDispatcher = fetchtest.Client

// mcpTestConfig is brokertest.NewConfig with the gateway on. The two PublicURLs
// differ (split-host topology) so a handler building a URL from the wrong one shows.
func mcpTestConfig() *core.Config {
	cfg := brokertest.NewConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	cfg.Server.MCP = core.MCPConfig{Addr: ":0", PublicURL: "https://gateway.example.com"}
	return cfg
}

// withAliceClaims attaches alice's verified claims, as gatewayAuth would.
func withAliceClaims(ctx context.Context) context.Context {
	claims := brokertest.AliceClaims()
	return core.CtxWithClaims(ctx, &claims)
}

// seededDispatcher carries /index.md in both memory worlds, so tenantGate's
// seeding takes the fast path and tests see no seed publishes unless asked.
func seededDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		Published: map[string]fetch.Result{
			"alice-w/index.md": {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Memory"}},
			"bob-w/index.md":   {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Memory"}},
		},
	}
}

// mcpResponse is a parsed JSON-RPC answer, or the raw status and body when
// the response is not 200.
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

// mcpSessionHeader carries the Streamable HTTP session id; the literal keeps
// mcpserver out of this file's imports.
const mcpSessionHeader = "Mcp-Session-Id"

// mcpRequest posts a JSON-RPC request at /mcp. An empty bearer omits the
// Authorization header.
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
	client := &http.Client{Timeout: brokertest.HTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
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

// initializeRequest is the MCP initialize payload every handshake uses.
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

// callToolReq mirrors the request shape mcp-go's transport produces.
func callToolReq(name string, args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
}
