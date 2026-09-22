package broker

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/mark3labs/mcp-go/mcp"
)

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

func TestHandleMarkFetchHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
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
	// Lean envelope by default: version and body, identity keys trimmed.
	if want := "status: ok\nversion: 3\n\n# hello\n"; text != want {
		t.Errorf("lean response = %q, want %q", text, want)
	}
	res, err = g.handleMarkFetch(ctx, callToolReq("mark_fetch", map[string]any{
		"url": "mark://team-a/foo.md", "verbose": true, "force": true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("verbose handleMarkFetch: %v %+v", err, res)
	}
	text = toolResultText(t, res)
	// Verbose: every metadata key verbatim, the proxy-fidelity rendering.
	for _, want := range []string{
		"status: ok",
		"version: 3",
		"modified: 2026-05-21T10:00:00Z",
		"etag: abc123",
		"content-hash: sha256-deadbeef",
		"# hello",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("verbose response missing %q\nfull:\n%s", want, text)
		}
	}
	// Dispatcher must have seen the worldName + path resolved
	// from the tool URL.
	if d.FetchCallCount() != 2 {
		t.Errorf("fetch dispatch count = %d, want 2", d.FetchCallCount())
	}
	call := d.FetchCalls[0]
	if call.Host != "team-a" {
		t.Errorf("dispatcher saw worldName=%q, want team-a", call.Host)
	}
	if call.Path != "/foo.md" {
		t.Errorf("dispatcher saw path=%q, want /foo.md", call.Path)
	}
	// Reads dispatch unauthenticated — the world's tokens.toml is
	// write-only in the knowledge-system flow, so the empty bearer
	// flows through and the document is returned.
	if call.Token != "" {
		t.Errorf("dispatcher saw token=%q, want empty (reads are open)", call.Token)
	}
}

func TestHandleMarkLookupHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		LookupFn: func(context.Context, fetch.LookupRequest) (fetch.Result, error) {
			row := render.LookupRow{Path: "/docs/auth.md", Importance: 0.9, Title: "Auth", Tags: []string{"auth"}}
			return fetchtest.Lookup("auth", "/docs/", "", row), nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkLookup(withAliceClaims(context.Background()), callToolReq("mark_lookup", map[string]any{
		"url":    "mark://team-a/docs/",
		"query":  "auth",
		"filter": "project=broker",
		"limit":  float64(5),
	}))
	if err != nil {
		t.Fatalf("handleMarkLookup: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %+v", res.Content)
	}
	text := toolResultText(t, res)
	for _, want := range []string{"status: ok", "matches: 1", "/docs/auth.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
	if len(d.LookupCalls) != 1 {
		t.Fatalf("lookup dispatch count = %d, want 1", len(d.LookupCalls))
	}
	call := d.LookupCalls[0]
	if call.Host != "team-a" || call.Scope != "/docs/" || call.Query != "auth" {
		t.Errorf("dispatcher saw worldName=%q scope=%q query=%q", call.Host, call.Scope, call.Query)
	}
	if call.Filter != "project=broker" || call.Limit != 5 {
		t.Errorf("dispatcher saw opts=%+v, want {Filter:project=broker Limit:5}", call)
	}
	// Reads dispatch unauthenticated — the empty bearer flows through.
	if call.Token != "" {
		t.Errorf("dispatcher saw token=%q, want empty (reads are open)", call.Token)
	}
}

func TestHandleMarkLookupRequiresQuery(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkLookup(withAliceClaims(context.Background()), callToolReq("mark_lookup", map[string]any{
		"url": "mark://team-a/",
	}))
	if err != nil {
		t.Fatalf("handleMarkLookup: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error for missing query")
	}
}

func TestHandleMarkListHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
			page := fetchtest.ListPage("/", "", "bar.md", "foo.md")
			page.Response.Metadata["modified"] = "2026-05-21T10:00:00Z"
			return page, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkList(withAliceClaims(context.Background()), callToolReq("mark_list", map[string]any{
		"url":              "mark://team-a/",
		"include_archived": true,
		"cursor":           "next",
		"page_size":        25,
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
	want := fetch.ListRequest{Host: d.ListCalls[0].Host, Path: d.ListCalls[0].Path, IncludeArchived: true, Cursor: "next", PageSize: 25}
	if len(d.ListCalls) != 1 || d.ListCalls[0] != want {
		t.Errorf("LIST options = %+v", d.ListCalls)
	}
}

func TestHandleMarkListRejectsFractionalPageSize(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkList(withAliceClaims(context.Background()), callToolReq("mark_list", map[string]any{
		"url":       "mark://team-a/",
		"page_size": 1.5,
	}))
	if err != nil || !res.IsError {
		t.Fatalf("fractional page_size = (%+v, %v), want tool error", res, err)
	}
}

func TestHandleMarkListRejectsRepeatedCursor(t *testing.T) {
	d := &fakeDispatcher{ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"entries": "0", "complete": "false", "next-cursor": "stuck",
			},
		}}, nil
	}}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	res, err := g.handleMarkList(withAliceClaims(context.Background()), callToolReq("mark_list", map[string]any{
		"url": "mark://team-a/", "cursor": "stuck",
	}))
	if err != nil {
		t.Fatalf("handleMarkList: %v", err)
	}
	if !res.IsError || !strings.Contains(toolResultText(t, res), "did not advance") {
		t.Fatalf("result = %+v", res)
	}
}

func TestHandleMarkListRejectsMissingContinuationCursor(t *testing.T) {
	d := &fakeDispatcher{ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"entries": "0", "complete": "false",
			},
		}}, nil
	}}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	res, err := g.handleMarkList(withAliceClaims(context.Background()), callToolReq("mark_list", map[string]any{
		"url": "mark://team-a/",
	}))
	if err != nil {
		t.Fatalf("handleMarkList: %v", err)
	}
	if !res.IsError || !strings.Contains(toolResultText(t, res), "missing or did not advance") {
		t.Fatalf("result = %+v", res)
	}
}

func TestHandleMarkVersionsHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
			return fetchtest.Golden(t, "versions"), nil
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
	pool := newWorldPool(cfg.worlds(), fetch.Options{})
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

// toolResultText extracts the first text-content chunk; tools always emit
// a single text content.
func toolResultText(t testing.TB, res *mcp.CallToolResult) string {
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
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
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
	ts := newTestMCPGatewayWith(t, cfg, &fakeVerifier{claims: aliceClaims()}, d)

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
	for _, want := range []string{"status: ok\nversion: 1\n\nbody via gateway"} {
		if !strings.Contains(text, want) {
			t.Errorf("end-to-end text missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkLookupBodyMatchFallbackNote(t *testing.T) {
	cfg := mcpTestConfig()
	var seen fetch.LookupRequest
	d := &fakeDispatcher{
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			seen = r
			return fetchtest.Lookup("hairpin", "/", ""), nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkLookup(withAliceClaims(context.Background()), callToolReq("mark_lookup", map[string]any{
		"url": "mark://team-a/", "query": "hairpin", "match": "body",
	}))
	if err != nil || res.IsError {
		t.Fatalf("handleMarkLookup: err %v res %+v", err, res)
	}
	if seen.Match != fetch.MatchBody {
		t.Errorf("dispatcher saw match %q, want body", seen.Match)
	}
	if text := toolResultText(t, res); !strings.Contains(text, fetch.CatalogFallbackNote) {
		t.Errorf("fallback note missing:\n%s", text)
	}
}
