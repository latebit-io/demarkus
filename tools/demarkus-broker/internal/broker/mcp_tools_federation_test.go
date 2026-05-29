package broker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/protocol"
	"k8s.io/client-go/kubernetes/fake"
)

// TestHandleMarkDiscoverHappyPath drives the simplest discover:
// world serves its agent manifest, broker forwards verbatim.
// Pins that the broker rewrites the path to the well-known
// manifest location (the agent only supplies the world URL).
func TestHandleMarkDiscoverHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
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
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
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

// indexBody builds a hash-index document body in the markdown
// table format the index package parses. Inline here so the
// resolve tests can construct arbitrary index scenarios without
// the test fake having to know about index.Build.
func indexBody(rows ...[3]string) string {
	var b strings.Builder
	b.WriteString("# Content Index\n\n")
	b.WriteString("| Hash | Server | Path |\n")
	b.WriteString("|------|--------|------|\n")
	for _, r := range rows {
		b.WriteString("| " + r[0] + " | " + r[1] + " | " + r[2] + " |\n")
	}
	return b.String()
}

// hash returned by indexBody rows must look like a valid hash
// path. Hard-code a few SHA-256 strings here so tests don't have
// to compute them; the broker uses protocol.IsHashPath to
// validate the input, but the worlds in these tests are fakes
// that just match by string.
const (
	testHashA = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testHashB = "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestHandleMarkResolveHappyPath: index pointing at team-a,
// team-a serves the content with a matching content-hash. First
// success wins.
func TestHandleMarkResolveHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			switch path {
			case "/hub/index.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body: indexBody(
						[3]string{testHashA, "mark://team-a", "/foo.md"},
					),
				}}, nil
			case "/" + testHashA:
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Metadata: map[string]string{
						"version":      "1",
						"modified":     "2026-05-21T13:00:00Z",
						"content-hash": testHashA,
					},
					Body: "# resolved content\n",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA,
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	for _, want := range []string{"status: ok", "content-hash: " + testHashA, "# resolved content"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkResolveInvalidHash(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  "not-a-real-hash",
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on invalid hash, want true")
	}
	if text := toolResultText(t, res); !strings.Contains(text, "invalid hash format") {
		t.Errorf("tool error message = %q, want it to name the hash format", text)
	}
}

func TestHandleMarkResolveMissingArgs(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash": testHashA,
		// index intentionally missing
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing index arg, want true")
	}
}

func TestHandleMarkResolveIndexFetchFailureBubbles(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{}, errors.New("dial team-a.team-a.svc.cluster.local:6309: connection refused")
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA,
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on index transport failure, want true")
	}
}

func TestHandleMarkResolveIndexNonOKStatus(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA,
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false when index returns non-ok, want true")
	}
	if text := toolResultText(t, res); !strings.Contains(text, "not-found") {
		t.Errorf("tool error should name the index status, got: %q", text)
	}
}

func TestHandleMarkResolveHashNotInIndex(t *testing.T) {
	// Index has entries, none match the requested hash.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body: indexBody(
					[3]string{testHashB, "mark://team-a", "/bar.md"},
				),
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA, // index has hashB only
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false when hash not in index, want true")
	}
	if text := toolResultText(t, res); !strings.Contains(text, "not found in index") {
		t.Errorf("tool error should say hash not found, got: %q", text)
	}
}

func TestHandleMarkResolveContentHashMismatchSkipsCandidate(t *testing.T) {
	// Index has two candidates for the same hash. First returns
	// content with a mismatched content-hash; broker skips it and
	// the second candidate (honest) wins.
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/*"},
		},
	})
	d := &fakeDispatcher{
		fetchFn: func(worldName, path, _ string) (fetch.Result, error) {
			switch {
			case path == "/hub/index.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body: indexBody(
						[3]string{testHashA, "mark://team-a", "/dishonest.md"},
						[3]string{testHashA, "mark://team-b", "/honest.md"},
					),
				}}, nil
			case worldName == "team-a" && path == "/"+testHashA:
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Metadata: map[string]string{
						"content-hash": testHashB, // mismatch!
					},
					Body: "evil content",
				}}, nil
			case worldName == "team-b" && path == "/"+testHashA:
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Metadata: map[string]string{
						"content-hash": testHashA, // honest
					},
					Body: "honest content",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA,
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true; should have fallen through to the honest candidate: %s", toolResultText(t, res))
	}
	if text := toolResultText(t, res); !strings.Contains(text, "honest content") {
		t.Errorf("resolve picked the wrong candidate; got:\n%s", text)
	}
}

func TestHandleMarkResolveCrossOrgCandidateSurfacesAsSkippedReason(t *testing.T) {
	// Index entry points at a world the broker has NO config for
	// (cross-org reference — slice 4a out-of-scope). Resolution
	// skips that candidate with a descriptive reason and reports
	// the final "could not resolve" naming the cross-org skip.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{}
	// Only the index fetch hits the fake; the cross-org server
	// goes through the real worldPool path. We need a real pool
	// for that branch — wire it via newGatewayWithDispatcher
	// using a *worldPool. But the *worldPool needs the index
	// fetch too. Cleanest: use the fake for the index and let
	// errWorldNotFound surface for the cross-org server. The
	// fake records every dispatch in fetchCalls; we keep it
	// simple by handling only the index path.
	d.fetchFn = func(worldName, path, _ string) (fetch.Result, error) {
		if worldName == "team-a" && path == "/hub/index.md" {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body: indexBody(
					[3]string{testHashA, "mark://outside-org", "/foo.md"},
				),
			}}, nil
		}
		// Unknown world for the cross-org candidate. Simulate
		// errWorldNotFound the way the production worldPool
		// would.
		return fetch.Result{}, &errWorldNotFound{worldName: worldName}
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA,
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false; cross-org-only index should fail to resolve")
	}
	if text := toolResultText(t, res); !strings.Contains(text, "not a knowledge-system world") {
		t.Errorf("tool error should explain the cross-org skip, got:\n%s", text)
	}
}

func TestHandleMarkResolveAllCandidatesFailReportsLast(t *testing.T) {
	// All candidates fail with different reasons. The aggregate
	// "could not resolve" message names the LAST failure so the
	// debugging operator sees the most-recent candidate's
	// complaint, not the first one (which may have been
	// retried-and-recovered semantics that hide the real cause).
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/*"},
		},
	})
	d := &fakeDispatcher{
		fetchFn: func(worldName, path, _ string) (fetch.Result, error) {
			switch {
			case path == "/hub/index.md":
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body: indexBody(
						[3]string{testHashA, "mark://team-a", "/foo.md"},
						[3]string{testHashA, "mark://team-b", "/bar.md"},
					),
				}}, nil
			case worldName == "team-a":
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			case worldName == "team-b":
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"content-hash": testHashB},
					Body:     "drift",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA,
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false when all candidates fail, want true")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "hash mismatch") {
		t.Errorf("expected the last failure (hash mismatch on team-b) to be named; got:\n%s", text)
	}
}

func TestHandleMarkResolveReusesAuthRetryAcrossCandidates(t *testing.T) {
	// Each candidate fetch goes through dispatchWithAuth, so a
	// candidate that returns `unauthorized` triggers the same
	// propagation-race retry the read handlers use. Pin that
	// behavior: candidate-A 401 → retry → ok; candidate-B never
	// touched because A resolved.
	cfg := mcpTestConfig()
	cfg.Server.MCP.FirstMintMaxAttempts = 3
	cfg.Server.MCP.FirstMintInitialBackoff = 1
	cfg.Server.MCP.FirstMintMaxBackoff = 2

	var attempt int32
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			if path == "/hub/index.md" {
				return fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body: indexBody(
						[3]string{testHashA, "mark://team-a", "/foo.md"},
					),
				}}, nil
			}
			n := atomic.AddInt32(&attempt, 1)
			if n == 1 {
				// Fresh-mint 401 — broker should retry.
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": testHashA},
				Body:     "resolved after retry",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkResolve(withAliceClaims(context.Background()), callToolReq("mark_resolve", map[string]any{
		"hash":  testHashA,
		"index": "mark://team-a/hub/index.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkResolve: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true after propagation-race retry should have resolved: %s", toolResultText(t, res))
	}
	if text := toolResultText(t, res); !strings.Contains(text, "resolved after retry") {
		t.Errorf("expected the retried success, got:\n%s", text)
	}
}

// TestMCPGatewayMarkDiscoverEndToEnd drives mark_discover
// through the full Streamable HTTP transport. One end-to-end
// test per slice pins the wire-shape contract; per-tool unit
// tests above cover the dispatch logic with finer granularity.
func TestMCPGatewayMarkDiscoverEndToEnd(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
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
