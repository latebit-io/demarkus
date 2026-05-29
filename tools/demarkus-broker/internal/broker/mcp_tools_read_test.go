package broker

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeDispatcher records dispatch calls and returns scripted
// fetch.Results. Wired into the gateway via Server.MCPGatewayWith
// so handler tests don't need a real demarkus-server / QUIC stack.
type fakeDispatcher struct {
	mu sync.Mutex

	fetchFn    func(worldName, path, token string) (fetch.Result, error)
	listFn     func(worldName, path, token string) (fetch.Result, error)
	versionsFn func(worldName, path, token string) (fetch.Result, error)
	publishFn  func(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	appendFn   func(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	archiveFn  func(worldName, path, token string) (fetch.Result, error)

	fetchCalls    []dispatchCall
	listCalls     []dispatchCall
	versionsCalls []dispatchCall
	publishCalls  []writeCall
	appendCalls   []writeCall
	archiveCalls  []dispatchCall
}

type dispatchCall struct {
	worldName string
	path      string
	token     string
}

// writeCall records a Publish/Append invocation. Captures the
// body + expected_version + meta so tests can assert what the
// broker forwarded to the world.
type writeCall struct {
	worldName       string
	path            string
	body            string
	token           string
	expectedVersion int
	meta            map[string]string
}

func (f *fakeDispatcher) Fetch(worldName, path, token string) (fetch.Result, error) {
	f.mu.Lock()
	f.fetchCalls = append(f.fetchCalls, dispatchCall{worldName, path, token})
	fn := f.fetchFn
	f.mu.Unlock()
	if fn == nil {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
	}
	return fn(worldName, path, token)
}

func (f *fakeDispatcher) List(worldName, path, token string) (fetch.Result, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, dispatchCall{worldName, path, token})
	fn := f.listFn
	f.mu.Unlock()
	if fn == nil {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
	}
	return fn(worldName, path, token)
}

func (f *fakeDispatcher) Versions(worldName, path, token string) (fetch.Result, error) {
	f.mu.Lock()
	f.versionsCalls = append(f.versionsCalls, dispatchCall{worldName, path, token})
	fn := f.versionsFn
	f.mu.Unlock()
	if fn == nil {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
	}
	return fn(worldName, path, token)
}

func (f *fakeDispatcher) Publish(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	f.mu.Lock()
	f.publishCalls = append(f.publishCalls, writeCall{worldName, path, body, token, expectedVersion, cloneMeta(meta)})
	fn := f.publishFn
	f.mu.Unlock()
	if fn == nil {
		return fetch.Result{Response: protocol.Response{
			Status:   protocol.StatusOK,
			Metadata: map[string]string{"version": "1"},
		}}, nil
	}
	return fn(worldName, path, body, token, expectedVersion, meta)
}

func (f *fakeDispatcher) Append(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	f.mu.Lock()
	f.appendCalls = append(f.appendCalls, writeCall{worldName, path, body, token, expectedVersion, cloneMeta(meta)})
	fn := f.appendFn
	f.mu.Unlock()
	if fn == nil {
		return fetch.Result{Response: protocol.Response{
			Status:   protocol.StatusOK,
			Metadata: map[string]string{"version": "2"},
		}}, nil
	}
	return fn(worldName, path, body, token, expectedVersion, meta)
}

// cloneMeta snapshots a publisher-metadata map so writeCall
// records an immutable copy. PR #147 review (CodeRabbit): the
// fakeDispatcher stored caller-owned maps by reference, so a
// post-call mutation of the same map (legal under fetch.Client's
// Publish signature — the client copies before sending, but a
// future broker handler that reused a map between calls would
// retroactively change recorded test history) could silently
// invalidate assertions written against the captured value.
// Cloning here is a test-only guard; production code paths still
// pass the original map down to fetch.Client.
func cloneMeta(meta map[string]string) map[string]string {
	if meta == nil {
		return nil
	}
	out := make(map[string]string, len(meta))
	maps.Copy(out, meta)
	return out
}

func (f *fakeDispatcher) Archive(worldName, path, token string) (fetch.Result, error) {
	f.mu.Lock()
	f.archiveCalls = append(f.archiveCalls, dispatchCall{worldName, path, token})
	fn := f.archiveFn
	f.mu.Unlock()
	if fn == nil {
		return fetch.Result{Response: protocol.Response{
			Status:   protocol.StatusArchived,
			Metadata: map[string]string{"version": "3"},
		}}, nil
	}
	return fn(worldName, path, token)
}

// fetchCallCount returns the number of Fetch calls recorded so
// far. Exposed for tests that assert mint coalescing and retry
// counts.
func (f *fakeDispatcher) fetchCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fetchCalls)
}

func TestParseToolURLDoubleSlashShape(t *testing.T) {
	world, path, err := parseToolURL("mark://team-a/foo.md")
	if err != nil {
		t.Fatalf("parseToolURL: %v", err)
	}
	if world != "team-a" {
		t.Errorf("world = %q, want team-a", world)
	}
	if path != "/foo.md" {
		t.Errorf("path = %q, want /foo.md", path)
	}
}

func TestParseToolURLBareHostBecomesRootPath(t *testing.T) {
	world, path, err := parseToolURL("mark://team-a")
	if err != nil {
		t.Fatalf("parseToolURL: %v", err)
	}
	if world != "team-a" {
		t.Errorf("world = %q, want team-a", world)
	}
	if path != "/" {
		t.Errorf("path = %q, want /", path)
	}
}

func TestParseToolURLTrailingSlash(t *testing.T) {
	world, path, err := parseToolURL("mark://team-a/")
	if err != nil {
		t.Fatalf("parseToolURL: %v", err)
	}
	if world != "team-a" {
		t.Errorf("world = %q, want team-a", world)
	}
	if path != "/" {
		t.Errorf("path = %q, want /", path)
	}
}

func TestParseToolURLRejectsWrongScheme(t *testing.T) {
	_, _, err := parseToolURL("https://team-a/foo.md")
	if err == nil {
		t.Fatal("parseToolURL accepted https:// scheme")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("err = %v, want message naming the scheme", err)
	}
}

func TestParseToolURLRejectsMissingWorld(t *testing.T) {
	_, _, err := parseToolURL("mark:///foo.md")
	if err == nil {
		t.Fatal("parseToolURL accepted URL with no world")
	}
	if !strings.Contains(err.Error(), "world name") {
		t.Errorf("err = %v, want message naming the missing world", err)
	}
}

func TestParseToolURLRejectsPort(t *testing.T) {
	// PR #146 review (CodeRabbit): url.URL.Hostname() silently
	// drops the port, so mark://team-a:7000/foo would have been
	// retargeted as team-a — masking an operator typo. Reject
	// outright instead.
	_, _, err := parseToolURL("mark://team-a:7000/foo.md")
	if err == nil {
		t.Fatal("parseToolURL accepted URL with port")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("err = %v, want message naming the port", err)
	}
}

func TestParseToolURLRejectsUserinfo(t *testing.T) {
	// PR #146 review (CodeRabbit): url.URL.Hostname() silently
	// drops userinfo. An agent embedding credentials in a URL
	// would have them dropped on the floor while the request
	// still went out as the bare worldName. Reject with a
	// useful error.
	_, _, err := parseToolURL("mark://eve@team-a/foo.md")
	if err == nil {
		t.Fatal("parseToolURL accepted URL with userinfo")
	}
	if !strings.Contains(err.Error(), "userinfo") {
		t.Errorf("err = %v, want message naming userinfo", err)
	}
}

func TestParseToolURLRejectsQuery(t *testing.T) {
	// PR #146 review (CodeRabbit, outside-diff): forwarding only
	// u.Path would silently strip `?rev=1`, giving the agent a
	// success response for a different target than it asked
	// for. Reject the shape outright.
	_, _, err := parseToolURL("mark://team-a/foo.md?rev=1")
	if err == nil {
		t.Fatal("parseToolURL accepted URL with query parameters")
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("err = %v, want message naming query parameters", err)
	}
}

func TestParseToolURLRejectsFragment(t *testing.T) {
	// PR #146 review (CodeRabbit, outside-diff): same silent-
	// rewrite class as the query case.
	_, _, err := parseToolURL("mark://team-a/foo.md#section-2")
	if err == nil {
		t.Fatal("parseToolURL accepted URL with fragment")
	}
	if !strings.Contains(err.Error(), "fragment") {
		t.Errorf("err = %v, want message naming fragment", err)
	}
}

func TestParseToolURLDeepPath(t *testing.T) {
	world, path, err := parseToolURL("mark://team-a/docs/architecture/index.md")
	if err != nil {
		t.Fatalf("parseToolURL: %v", err)
	}
	if world != "team-a" {
		t.Errorf("world = %q, want team-a", world)
	}
	if path != "/docs/architecture/index.md" {
		t.Errorf("path = %q, want /docs/architecture/index.md", path)
	}
}

// newGatewayWithDispatcher builds an mcpGateway with the supplied
// dispatcher, bypassing the HTTP layer. Used for unit tests that
// drive handlers directly through their Go signatures. The server's
// clock is pinned to a fixed instant so any time-dependent assertion
// stays deterministic across runs.
func newGatewayWithDispatcher(t *testing.T, cfg *Config, d worldDispatcher) *mcpGateway {
	t.Helper()
	signer := newTestSigner(t)
	verifier := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	k8s := fake.NewSimpleClientset()
	srv := NewServer(cfg, signer, verifier, k8s, nil, nil, nil)
	srv.clock = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	return newMCPGateway(srv, "test", d)
}

// callToolReq builds a mcp.CallToolRequest with the given
// arguments. Mirrors the shape mcp-go's transport produces so
// handler tests exercise the same code path real traffic does.
func callToolReq(name string, args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
}

// withAliceClaims returns ctx with verified claims attached,
// matching what gatewayAuth installs in production traffic.
func withAliceClaims(ctx context.Context) context.Context {
	return ctxWithClaims(ctx, Claims{
		Subject:       "google|alice",
		Email:         "alice@example.com",
		EmailVerified: true,
	})
}

func TestHandleMarkFetchHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":      "3",
					"modified":     "2026-05-21T10:00:00Z",
					"etag":         "abc123",
					"content-hash": "sha256-deadbeef",
				},
				Body: "# hello\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	ctx := withAliceClaims(context.Background())
	res, err := g.handleMarkFetch(ctx, callToolReq("mark_fetch", map[string]any{
		"url": "mark://team-a/foo.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkFetch: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %+v", res.Content)
	}
	text := toolResultText(t, res)
	// Body + every metadata key must appear verbatim — proxy
	// fidelity: the broker does not transform the world's
	// response.
	for _, want := range []string{
		"status: ok",
		"version: 3",
		"modified: 2026-05-21T10:00:00Z",
		"etag: abc123",
		"content-hash: sha256-deadbeef",
		"# hello",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
	// Dispatcher must have seen the worldName + path resolved
	// from the tool URL.
	if d.fetchCallCount() != 1 {
		t.Errorf("fetch dispatch count = %d, want 1", d.fetchCallCount())
	}
	call := d.fetchCalls[0]
	if call.worldName != "team-a" {
		t.Errorf("dispatcher saw worldName=%q, want team-a", call.worldName)
	}
	if call.path != "/foo.md" {
		t.Errorf("dispatcher saw path=%q, want /foo.md", call.path)
	}
	// Reads dispatch unauthenticated — the world's tokens.toml is
	// write-only in the knowledge-system flow, so the empty bearer
	// flows through and the document is returned.
	if call.token != "" {
		t.Errorf("dispatcher saw token=%q, want empty (reads are open)", call.token)
	}
}

func TestHandleMarkListHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"modified": "2026-05-21T10:00:00Z"},
				Body:     "foo.md\nbar.md\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkList(withAliceClaims(context.Background()), callToolReq("mark_list", map[string]any{
		"url": "mark://team-a/",
	}))
	if err != nil {
		t.Fatalf("handleMarkList: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true")
	}
	text := toolResultText(t, res)
	for _, want := range []string{"status: ok", "modified: 2026-05-21T10:00:00Z", "foo.md", "bar.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkVersionsHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		versionsFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"total":       "3",
					"current":     "3",
					"chain-valid": "true",
				},
				Body: "v1: 2026-05-19T10:00:00Z\nv2: 2026-05-20T10:00:00Z\nv3: 2026-05-21T10:00:00Z\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkVersions(withAliceClaims(context.Background()), callToolReq("mark_versions", map[string]any{
		"url": "mark://team-a/foo.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkVersions: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true")
	}
	text := toolResultText(t, res)
	for _, want := range []string{"status: ok", "total: 3", "current: 3", "chain-valid: true"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkFetchMissingURLArg(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkFetch(withAliceClaims(context.Background()), callToolReq("mark_fetch", nil))
	if err != nil {
		t.Fatalf("handleMarkFetch: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing url, want true")
	}
}

func TestHandleMarkFetchInvalidURL(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkFetch(withAliceClaims(context.Background()), callToolReq("mark_fetch", map[string]any{
		"url": "https://wrong-scheme/foo",
	}))
	if err != nil {
		t.Fatalf("handleMarkFetch: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on https:// URL, want true")
	}
}

func TestHandleMarkFetchUnknownWorld(t *testing.T) {
	// fakeDispatcher accepts any worldName, so the not-found
	// path needs the real worldPool path. Use a Config with a
	// single world named team-a; targeting team-b must surface
	// errWorldNotFound through the handler.
	cfg := mcpTestConfig()
	pool := newWorldPool(cfg, fetch.Options{})
	g := newGatewayWithDispatcher(t, cfg, pool)
	res, err := g.handleMarkFetch(withAliceClaims(context.Background()), callToolReq("mark_fetch", map[string]any{
		"url": "mark://team-b/foo.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkFetch: %v", err)
	}
	if !res.IsError {
		t.Fatal("isError = false on unknown world, want true")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "team-b") {
		t.Errorf("tool error missing world name: %q", text)
	}
}

// formatResultReference is a verbatim copy of the local
// client/cmd/demarkus-mcp formatResult helper. Drift between the
// broker's formatToolResult and this reference would silently
// break the byte-for-byte proxy contract; the parity test below
// catches it. If the local server ever changes its formatter,
// this copy must move in lockstep — at which point hoisting both
// into a shared package becomes the right call.
//
// The remaining-keys sort below mirrors the determinism fix
// applied in both implementations after PR #146 review surfaced
// nondeterministic map iteration order.
func formatResultReference(r fetch.Result, keys ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", r.Response.Status)
	shown := make(map[string]bool, len(keys))
	for _, key := range keys {
		if v, ok := r.Response.Metadata[key]; ok {
			fmt.Fprintf(&b, "%s: %s\n", key, v)
			shown[key] = true
		}
	}
	remaining := make([]string, 0, len(r.Response.Metadata))
	for k := range r.Response.Metadata {
		if !shown[k] {
			remaining = append(remaining, k)
		}
	}
	sort.Strings(remaining)
	for _, k := range remaining {
		fmt.Fprintf(&b, "%s: %s\n", k, r.Response.Metadata[k])
	}
	if r.Response.Body != "" {
		b.WriteString("\n")
		b.WriteString(r.Response.Body)
	}
	return b.String()
}

func TestFormatToolResultDeterministicRemainingOrder(t *testing.T) {
	// Go's map iteration is randomized; running the formatter
	// many times on the same Result must produce one canonical
	// output, otherwise the byte-for-byte proxy contract is a
	// statistical claim rather than a property. Five keys none
	// of which are in the explicit `keys` argument exercises
	// the remaining-order code path. PR #146 review surfaced
	// the bug; this test pins the fix.
	r := fetch.Result{Response: protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"zeta":  "1",
			"alpha": "2",
			"beta":  "3",
			"delta": "4",
			"gamma": "5",
		},
		Body: "body",
	}}
	first := formatToolResult(r)
	for range 20 {
		if got := formatToolResult(r); got != first {
			t.Fatalf("formatToolResult nondeterministic across runs\nfirst:\n%s\nlater:\n%s", first, got)
		}
	}
	// Pin the actual ordering — alpha, beta, delta, gamma, zeta.
	wantOrder := []string{"alpha", "beta", "delta", "gamma", "zeta"}
	idx := 0
	for _, key := range wantOrder {
		next := strings.Index(first[idx:], key)
		if next < 0 {
			t.Fatalf("expected key %q in remaining order, not found at or after offset %d\nfull:\n%s", key, idx, first)
		}
		idx += next
	}
}

func TestProxyFidelityFormatMatchesLocalServer(t *testing.T) {
	// The broker is a byte-for-byte proxy: a document fetched
	// through the gateway and the same document fetched directly
	// via the local demarkus-mcp must produce identical text. We
	// pin equality against formatResultReference (the local
	// server's helper) here so any drift in formatToolResult
	// trips this test before it ships to production.
	cases := []struct {
		name string
		r    fetch.Result
		keys []string
	}{
		{
			name: "ok with multiple metadata keys",
			r: fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":      "7",
					"modified":     "2026-05-21T10:00:00Z",
					"etag":         "abc-123",
					"content-hash": "sha256-deadbeef",
				},
				Body: "# title\nbody body body\n",
			}},
			keys: []string{"version", "modified", "etag"},
		},
		{
			name: "no body",
			r: fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusArchived,
				Metadata: map[string]string{"version": "3"},
			}},
			keys: []string{"version"},
		},
		{
			name: "unauthorized status with no metadata",
			r: fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusUnauthorized,
				Metadata: map[string]string{},
			}},
			keys: nil,
		},
		{
			name: "metadata key not in keys still appears",
			r: fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1", "content-hash": "sha256-cafe"},
				Body:     "x",
			}},
			keys: []string{"version"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatToolResult(tc.r, tc.keys...)
			want := formatResultReference(tc.r, tc.keys...)
			if got != want {
				t.Errorf("formatToolResult drift from local server\nbroker:\n%s\nlocal:\n%s", got, want)
			}
		})
	}
}

// toolResultText extracts the first text-content chunk from a
// CallToolResult so assertions can inspect the rendered output.
// Tools always emit a single text content; if that ever changes
// the test needs to fan out, but until then a single helper covers
// every assertion.
func toolResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("tool result has no content")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("tool content[0] is not TextContent: %T", res.Content[0])
	}
	return tc.Text
}

// TestMCPGatewayMarkFetchEndToEnd drives the read handler through
// the full Streamable HTTP transport including gatewayAuth and
// session keying so we have one test that pins the wire-shape
// contract (status code, JSON-RPC envelope, headers). The unit
// tests above cover the dispatch logic with finer granularity.
func TestMCPGatewayMarkFetchEndToEnd(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":      "1",
					"modified":     "2026-05-21T10:00:00Z",
					"etag":         "abc",
					"content-hash": "sha256-deadbeef",
				},
				Body: "body via gateway",
			}}, nil
		},
	}
	verifier := &fakeVerifier{claims: Claims{
		Subject: "google|alice", Email: "alice@example.com", EmailVerified: true,
	}}
	signer := newTestSigner(t)
	k8s := fake.NewSimpleClientset()
	brokerSrv := NewServer(cfg, signer, verifier, k8s, nil, nil, nil)
	ts := httptest.NewServer(brokerSrv.MCPGatewayWith("test", d))
	t.Cleanup(ts.Close)

	initR := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if initR.HTTPStatus != http.StatusOK {
		t.Fatalf("initialize: status = %d, body = %s", initR.HTTPStatus, initR.RawBody)
	}
	sessionID := initR.Headers[mcpSessionHeader]
	if sessionID == "" {
		t.Fatalf("no session header on initialize")
	}

	resp := mcpRequest(t, ts.URL, "alice-token", sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "mark_fetch",
			"arguments": map[string]any{"url": "mark://team-a/foo.md"},
		},
	})
	if resp.HTTPStatus != http.StatusOK {
		t.Fatalf("tools/call: status = %d, body = %s", resp.HTTPStatus, resp.RawBody)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call returned JSON-RPC error: %+v", resp.Error)
	}
	isErr, _ := resp.Result["isError"].(bool)
	if isErr {
		t.Fatalf("tools/call result.isError = true: %+v", resp.Result)
	}
	contents, _ := resp.Result["content"].([]any)
	if len(contents) == 0 {
		t.Fatalf("content empty")
	}
	first, _ := contents[0].(map[string]any)
	text, _ := first["text"].(string)
	for _, want := range []string{"status: ok", "version: 1", "etag: abc", "content-hash: sha256-deadbeef", "body via gateway"} {
		if !strings.Contains(text, want) {
			t.Errorf("end-to-end text missing %q\nfull:\n%s", want, text)
		}
	}
}
