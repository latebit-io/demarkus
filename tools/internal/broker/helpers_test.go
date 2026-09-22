package broker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	"k8s.io/client-go/kubernetes/fake"
)

// Shared fixtures: configs, fakes and constructors every test file builds on.

const (
	testBrokerNS = "broker-ns"
	// testIssuancesNS names the Secret the removed issuance subsystem wrote;
	// install and sweeper tests assert it never appears.
	testIssuancesNS = "broker-issuances"
)

// testHTTPTimeout fails a deadlocked handler in seconds, not at the suite timeout.
const testHTTPTimeout = 5 * time.Second

// fixedTestTime is the instant every gateway fixture clock starts at.
var fixedTestTime = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

func testConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:            ":0",
			CookieKey:       "dGVzdC1rZXktMTIzNDU2Nzg5MGFi",
			BrokerNamespace: testBrokerNS,
			StateTTL:        5 * time.Minute,
		},
		OIDC: OIDCConfig{
			Issuer: "https://idp", ClientID: "c", ClientSecret: "s", RedirectURL: "r",
		},
		Worlds: []WorldConfig{
			{
				Name:         "team-a",
				Namespace:    "team-a",
				TokensSecret: "team-a-tokens",
				Allow:        AllowConfig{Domains: []string{"example.com"}},
				DefaultToken: TokenScope{
					Paths: []string{"/team-a/*"},
				},
			},
		},
	}
}

// mcpTestConfig is testConfig with the gateway on. The two PublicURLs differ
// (split-host topology) so a handler building a URL from the wrong one shows.
func mcpTestConfig() *Config {
	cfg := testConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	cfg.Server.MCP = MCPConfig{Addr: ":0", PublicURL: "https://gateway.example.com"}
	return cfg
}

// memoryTestConfig provisions two static tenants: alice owns alice-w,
// bob owns bob-w.
func memoryTestConfig() *Config {
	cfg := testConfig()
	cfg.Server.MCP = MCPConfig{Addr: ":0", PublicURL: "https://memory.example.com"}
	cfg.Worlds = []WorldConfig{
		{
			Name: "alice-w", Namespace: "alice-w", TokensSecret: "alice-w-tokens",
			Allow: AllowConfig{Emails: []string{"alice@example.com"}},
		},
		{
			Name: "bob-w", Namespace: "bob-w", TokensSecret: "bob-w-tokens",
			Allow: AllowConfig{Emails: []string{"bob@example.com"}},
		},
	}
	return cfg
}

// fakeClock is a settable clock shared by a fixture's server and gateway.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// fakeVerifier implements Verifier with predetermined claims, uncoupled
// from the OIDC library.
type fakeVerifier struct {
	authURL     string
	claims      Claims
	rawIDToken  string
	accessToken string
	expiry      time.Time
	exchErr     error
	verifyFn    func(raw string) (Claims, error) // optional per-token behavior for VerifyIDToken
}

func (f *fakeVerifier) AuthCodeURL(state string) string {
	if f.authURL == "" {
		return "https://idp.example.com/authorize?state=" + url.QueryEscape(state)
	}
	if strings.Contains(f.authURL, "?") {
		return f.authURL + "&state=" + url.QueryEscape(state)
	}
	return f.authURL + "?state=" + url.QueryEscape(state)
}

func (f *fakeVerifier) Exchange(_ context.Context, _ string) (ExchangeResult, error) {
	if f.exchErr != nil {
		return ExchangeResult{}, f.exchErr
	}
	return ExchangeResult{
		Claims:      f.claims,
		RawIDToken:  f.rawIDToken,
		AccessToken: f.accessToken,
		Expiry:      f.expiry,
	}, nil
}

func (f *fakeVerifier) VerifyIDToken(_ context.Context, raw string) (Claims, error) {
	if f.verifyFn != nil {
		return f.verifyFn(raw)
	}
	return f.claims, nil
}

// aliceClaims is the identity most gateway tests run as.
func aliceClaims() Claims {
	return Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}
}

// withAliceClaims attaches alice's verified claims, as gatewayAuth would.
func withAliceClaims(ctx context.Context) context.Context {
	claims := aliceClaims()
	return ctxWithClaims(ctx, &claims)
}

// fakeDispatcher is the shared scriptable client; see client/fetchtest.
type fakeDispatcher = fetchtest.Client

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

func newTestSigner(t testing.TB) *Signer {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	s, err := NewSigner(key)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// generateTestSigningKey is a fresh ECDSA P-256 key as PKCS#8 PEM,
// ephemeral per test.
func generateTestSigningKey(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// generateTestSigningKeySEC1 is the same key in the legacy SEC1 shape, for
// NewIDTokenSigner's other branch.
func generateTestSigningKeySEC1(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal SEC1: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func newTestIDTokenSigner(t *testing.T) *IDTokenSigner {
	t.Helper()
	s, err := NewIDTokenSigner(generateTestSigningKey(t))
	if err != nil {
		t.Fatalf("NewIDTokenSigner: %v", err)
	}
	return s
}

// testServerDeps is ServerDeps over a fake clientset with the config's rate
// limits, no discovery and no id_token signer; see signed.
func testServerDeps(t testing.TB, cfg *Config, verifier Verifier, k8s *fake.Clientset) ServerDeps {
	t.Helper()
	deps := ServerDeps{Signer: newTestSigner(t), Verifier: verifier, Store: NewK8sSecretStore(k8s)}
	deps.SubjectLimiter, deps.LoginLimiter = newRateLimits(&cfg.RateLimit)
	return deps
}

// signed adds an id_token signer and composes the verifier, as Run does, so
// the refresh grant, JWKS and the broker signed bearer leg are live.
func (d ServerDeps) signed(cfg *Config, signer *IDTokenSigner) ServerDeps {
	d.IDTokenSigner = signer
	d.Verifier = verifierWith(d.Verifier, signer, cfg.Server.PublicURL)
	return d
}

// newTestServer serves the management API over TLS, since the state cookie
// is Secure and scoped to /auth/callback. The clock stays at time.Now: a
// pinned date would expire the OIDC state cookie under any later wall clock.
func newTestServer(t *testing.T, cfg *Config, verifier Verifier, k8s *fake.Clientset) (testSrv *httptest.Server, brokerSrv *Server) {
	t.Helper()
	return newTestServerWithSigner(t, cfg, verifier, k8s, nil)
}

// newTestServerWithSigner is newTestServer with an id_token signer.
func newTestServerWithSigner(t *testing.T, cfg *Config, verifier Verifier, k8s *fake.Clientset, signer *IDTokenSigner) (testSrv *httptest.Server, brokerSrv *Server) {
	t.Helper()
	brokerSrv = NewServer(cfg, testServerDeps(t, cfg, verifier, k8s).signed(cfg, signer))
	testSrv = httptest.NewTLSServer(brokerSrv.Routes())
	t.Cleanup(testSrv.Close)
	return testSrv, brokerSrv
}

// testClient is a per-test copy of the server's client with the timeout
// applied; httptest returns one shared instance, so Jar and CheckRedirect
// changes would otherwise leak between tests.
func testClient(srv *httptest.Server) *http.Client {
	base := srv.Client()
	c := *base
	c.Timeout = testHTTPTimeout
	return &c
}

// gatewayFixture is one set of ServerDeps wired the way Run wires them: the
// gateway and the management API (srv) share the verifier, clock and subject
// limiter. The clock starts at fixedTestTime.
type gatewayFixture struct {
	cfg         *Config
	deps        ServerDeps
	srv         *Server
	store       SecretStore
	clock       *fakeClock
	profile     *GatewayProfile
	provisioner *Provisioner
}

func newGatewayFixture(t testing.TB, cfg *Config, verifier Verifier, profile *GatewayProfile) *gatewayFixture {
	t.Helper()
	return gatewayFixtureFor(t, cfg, ServerDeps{Verifier: verifier}, profile)
}

// gatewayFixtureFor builds the fixture around deps, filling the signer, the
// store, the clock, the limiters and the profile's scoping.
func gatewayFixtureFor(t testing.TB, cfg *Config, deps ServerDeps, profile *GatewayProfile) *gatewayFixture {
	t.Helper()
	store := NewK8sSecretStore(fake.NewSimpleClientset())
	clock := &fakeClock{now: fixedTestTime}
	deps.Signer, deps.Store, deps.Log, deps.Clock = newTestSigner(t), store, slog.Default(), clock.Now
	deps.SubjectLimiter, deps.LoginLimiter = newRateLimits(&cfg.RateLimit)
	deps.TenantScoped = profile.TenantScoped
	return &gatewayFixture{cfg: cfg, deps: deps, srv: NewServer(cfg, deps), store: store, clock: clock, profile: profile}
}

// enableProvisioning wires dynamic tenant provisioning over the fixture's
// store and clock.
func (f *gatewayFixture) enableProvisioning(buckets BucketCreator) *Provisioner {
	f.provisioner = newProvisioner(f.cfg, provisionerDeps{Store: f.store, Buckets: buckets, Log: f.deps.Log, Clock: f.clock.Now})
	return f.provisioner
}

// gateway builds the gateway around d, for tests that call handlers directly.
func (f *gatewayFixture) gateway(d worldDispatcher) *mcpGateway {
	return newMCPGateway(gatewayDepsFor(f.cfg, &f.deps, f.store, f.provisioner), "test", d, f.profile)
}

// serve hosts the gateway's routes, for tests that drive the HTTP transport.
func (f *gatewayFixture) serve(t testing.TB, d worldDispatcher) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(f.gateway(d).Routes())
	t.Cleanup(ts.Close)
	return ts
}

// newGatewayWithDispatcher is a knowledge gateway run as alice, in process.
func newGatewayWithDispatcher(t testing.TB, cfg *Config, d worldDispatcher) *mcpGateway {
	t.Helper()
	return newGatewayFixture(t, cfg, &fakeVerifier{claims: aliceClaims()}, KnowledgeGatewayProfile()).gateway(d)
}

// newMemoryGateway is a memory gateway run as alice, in process.
func newMemoryGateway(t testing.TB, cfg *Config, d worldDispatcher) *mcpGateway {
	t.Helper()
	return newGatewayFixture(t, cfg, &fakeVerifier{claims: aliceClaims()}, MemoryGatewayProfile()).gateway(d)
}

// memoryGatewayWithProvisioning is newMemoryGateway with dynamic provisioning
// over the fake Secret store.
func memoryGatewayWithProvisioning(t *testing.T, cfg *Config, d worldDispatcher) (*mcpGateway, *fakeBuckets) {
	t.Helper()
	f := newGatewayFixture(t, cfg, &fakeVerifier{claims: aliceClaims()}, MemoryGatewayProfile())
	buckets := &fakeBuckets{}
	f.enableProvisioning(buckets)
	return f.gateway(d), buckets
}

// newTestMCPGateway serves a knowledge gateway over HTTP with the production
// world pool, for tests that never reach a world.
func newTestMCPGateway(t *testing.T, cfg *Config, verifier Verifier) *httptest.Server {
	t.Helper()
	pool := newWorldPool(cfg.worlds(), fetch.Options{})
	t.Cleanup(pool.Close)
	return newTestMCPGatewayWith(t, cfg, verifier, pool)
}

// newTestMCPGatewayWith serves a knowledge gateway over HTTP around d.
func newTestMCPGatewayWith(t *testing.T, cfg *Config, verifier Verifier, d worldDispatcher) *httptest.Server {
	t.Helper()
	return newGatewayFixture(t, cfg, verifier, KnowledgeGatewayProfile()).serve(t, d)
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
	client := &http.Client{Timeout: testHTTPTimeout}
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
