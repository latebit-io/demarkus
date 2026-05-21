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

// TestHandleMarkPublishHappyPath drives the simplest publish:
// known expected_version, default on_conflict, no validation
// errors. Pins the byte-for-byte proxy contract — body + version
// + modified pass through the broker unchanged.
func TestHandleMarkPublishHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":        "4",
					"modified":       "2026-05-21T10:00:00Z",
					"server-version": "1.0",
				},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "# new content\n",
		"expected_version": float64(3),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	for _, want := range []string{"status: ok", "version: 4", "modified: 2026-05-21T10:00:00Z", "server-version: 1.0"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
	if len(d.publishCalls) != 1 {
		t.Fatalf("publish dispatch count = %d, want 1", len(d.publishCalls))
	}
	call := d.publishCalls[0]
	if call.worldName != "team-a" {
		t.Errorf("worldName = %q, want team-a", call.worldName)
	}
	if call.path != "/foo.md" {
		t.Errorf("path = %q, want /foo.md", call.path)
	}
	if call.body != "# new content\n" {
		t.Errorf("body = %q, want forwarded verbatim", call.body)
	}
	if call.expectedVersion != 3 {
		t.Errorf("expectedVersion = %d, want 3", call.expectedVersion)
	}
	if call.meta["agent"] != "alice@example.com" {
		t.Errorf("publisher meta agent = %q, want canonical email", call.meta["agent"])
	}
}

func TestHandleMarkPublishMissingExpectedVersion(t *testing.T) {
	// expected_version is a REQUIRED field on mark_publish (the
	// auto-resolve convenience lives on mark_append only). Missing
	// it must surface as a clear tool error, not be silently
	// defaulted to 0 (which would mean "create-only" and confuse
	// agents trying to update).
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":  "mark://team-a/foo.md",
		"body": "hello",
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing expected_version, want true")
	}
	if text := toolResultText(t, res); !strings.Contains(text, "expected_version") {
		t.Errorf("tool error message = %q, want it to name expected_version", text)
	}
}

func TestHandleMarkPublishNegativeExpectedVersion(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(-1),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on negative expected_version, want true")
	}
}

func TestHandleMarkPublishConflictPassesThroughVerbatim(t *testing.T) {
	// version-mismatch on the world surfaces as `conflict`
	// status. The broker forwards verbatim — body + version +
	// server-version unchanged — so the agent can act on the
	// conflict envelope the same way against a brokered or
	// direct-QUIC world.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusConflict,
				Metadata: map[string]string{
					"version":        "7",
					"server-version": "7",
				},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "stale body",
		"expected_version": float64(5),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Errorf("isError = true on conflict (must forward verbatim, not turn into a tool error): %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "status: conflict") {
		t.Errorf("expected status: conflict verbatim in output, got:\n%s", text)
	}
	if !strings.Contains(text, "server-version: 7") {
		t.Errorf("expected server-version: 7 in output, got:\n%s", text)
	}
}

func TestHandleMarkPublishOnConflictMergeRejectedUntilSlice6(t *testing.T) {
	// Slice 3 deliberately does NOT ship the merge candidate
	// flow (Slice 6 lands it). Agents calling with
	// on_conflict="merge" need to know the surface gap so they
	// can either pass "fail" or wait. Silently accepting and
	// running the "fail" path would be a bait-and-switch — the
	// agent thinks merge is being attempted when it isn't.
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(3),
		"on_conflict":      "merge",
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on on_conflict=merge, want true (Slice 6)")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "Slice 6") {
		t.Errorf("tool error %q must point at Slice 6 of the plan so agents know when to retry", text)
	}
}

func TestHandleMarkPublishOnConflictUnknownRejected(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(3),
		"on_conflict":      "explode",
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on unknown on_conflict, want true")
	}
}

func TestHandleMarkPublishNotPermittedSurfacesAsToolError(t *testing.T) {
	// World-side RBAC failure (token lacks `publish` op or the
	// path is outside the token's allowed paths) returns
	// `not-permitted`. The plan v5 contract says it forwards
	// through the proxy as a regular response, NOT a tool error
	// — the local demarkus-mcp behaves the same way (just shows
	// "status: not-permitted" in the formatted text). Pin that.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusNotPermitted,
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Errorf("isError = true on not-permitted (proxy contract: forward verbatim): %s", toolResultText(t, res))
	}
	if text := toolResultText(t, res); !strings.Contains(text, "status: not-permitted") {
		t.Errorf("expected not-permitted in output, got:\n%s", text)
	}
}

func TestHandleMarkPublishTransportErrorFailsLoud(t *testing.T) {
	// Transport-layer failures (world unreachable, QUIC dial
	// timeout, etc.) surface as the dispatcher returning a
	// non-nil error. That MUST become a tool error so the agent
	// distinguishes "the world said no" from "we never reached
	// the world." Pin the distinction.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{}, errors.New("dial team-a.team-a.svc.cluster.local:6309: connection refused")
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on transport error, want true")
	}
}

func TestFakeDispatcherSnapshotsMetaOnCapture(t *testing.T) {
	// PR #147 review (CodeRabbit): the fakeDispatcher used to
	// store the caller-supplied meta map by reference. A
	// post-call mutation of that map would silently change the
	// historical writeCall record. The fake now snapshots; pin
	// the property so a future refactor that drops cloneMeta
	// fails this test instead of producing flaky assertions.
	d := &fakeDispatcher{}
	meta := map[string]string{"agent": "alice@example.com"}
	if _, err := d.Publish("team-a", "/foo.md", "hello", "token", 0, meta); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Caller mutates AFTER the call returns — simulating a
	// handler reusing one map across multiple dispatcher calls,
	// or a future code path that builds meta lazily.
	meta["agent"] = "mallory@evil.example"
	meta["injected"] = "should-not-appear"

	if got := d.publishCalls[0].meta["agent"]; got != "alice@example.com" {
		t.Errorf("recorded meta[agent] = %q, want alice@example.com (mutation after Publish leaked into history)", got)
	}
	if _, ok := d.publishCalls[0].meta["injected"]; ok {
		t.Error("recorded meta inherited a key added after Publish — snapshot broken")
	}
}

// TestHandleMarkAppendHappyPath drives an append with an
// explicit expected_version. Same byte-for-byte forwarding
// invariant as publish.
func TestHandleMarkAppendHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		appendFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":        "8",
					"modified":       "2026-05-21T11:00:00Z",
					"server-version": "1.0",
				},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkAppend(withAliceClaims(context.Background()), callToolReq("mark_append", map[string]any{
		"url":              "mark://team-a/journal.md",
		"body":             "## new entry\n",
		"expected_version": float64(7),
	}))
	if err != nil {
		t.Fatalf("handleMarkAppend: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	if len(d.appendCalls) != 1 {
		t.Fatalf("append calls = %d, want 1", len(d.appendCalls))
	}
	if got := d.appendCalls[0].expectedVersion; got != 7 {
		t.Errorf("dispatcher saw expectedVersion=%d, want 7", got)
	}
	if got := d.appendCalls[0].body; got != "## new entry\n" {
		t.Errorf("dispatcher saw body=%q, want forwarded verbatim", got)
	}
}

func TestHandleMarkAppendAutoResolvesViaVersions(t *testing.T) {
	// expected_version omitted → broker fetches VERSIONS, reads
	// `current`, and uses that for APPEND. The local
	// demarkus-mcp does the same; pin parity.
	cfg := mcpTestConfig()
	var versionsHits, appendHits int32
	d := &fakeDispatcher{
		versionsFn: func(_, _, _ string) (fetch.Result, error) {
			atomic.AddInt32(&versionsHits, 1)
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"total":   "12",
					"current": "12",
				},
			}}, nil
		},
		appendFn: func(_, _, _, _ string, expectedVersion int, _ map[string]string) (fetch.Result, error) {
			atomic.AddInt32(&appendHits, 1)
			if expectedVersion != 12 {
				t.Errorf("append dispatched with expectedVersion=%d, want 12 (resolved from VERSIONS)", expectedVersion)
			}
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":        "13",
					"server-version": "1.0",
				},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkAppend(withAliceClaims(context.Background()), callToolReq("mark_append", map[string]any{
		"url":  "mark://team-a/journal.md",
		"body": "## entry\n",
		// expected_version intentionally omitted
	}))
	if err != nil {
		t.Fatalf("handleMarkAppend: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	if v := atomic.LoadInt32(&versionsHits); v != 1 {
		t.Errorf("versions dispatch count = %d, want 1", v)
	}
	if a := atomic.LoadInt32(&appendHits); a != 1 {
		t.Errorf("append dispatch count = %d, want 1", a)
	}
}

func TestHandleMarkAppendVersionsFailureBubbles(t *testing.T) {
	// VERSIONS auto-resolve hits an unreachable world or the
	// world returns non-ok. Either way the broker must NOT
	// silently proceed with expected_version=0 (which would
	// mean "create-only" and overwrite intent). Surface as a
	// tool error so the agent can retry with an explicit
	// version.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		versionsFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkAppend(withAliceClaims(context.Background()), callToolReq("mark_append", map[string]any{
		"url":  "mark://team-a/journal.md",
		"body": "## entry\n",
	}))
	if err != nil {
		t.Fatalf("handleMarkAppend: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false when VERSIONS auto-resolve returns not-found, want true")
	}
	if len(d.appendCalls) != 0 {
		t.Errorf("append dispatched %d times after VERSIONS failure, want 0", len(d.appendCalls))
	}
}

func TestHandleMarkAppendMissingBody(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkAppend(withAliceClaims(context.Background()), callToolReq("mark_append", map[string]any{
		"url":              "mark://team-a/foo.md",
		"expected_version": float64(1),
	}))
	if err != nil {
		t.Fatalf("handleMarkAppend: %v", err)
	}
	if !res.IsError {
		t.Error("isError = false on missing body, want true")
	}
}

func TestHandleMarkArchiveHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		archiveFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusArchived,
				Metadata: map[string]string{"version": "9"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkArchive(withAliceClaims(context.Background()), callToolReq("mark_archive", map[string]any{
		"url": "mark://team-a/obsolete.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkArchive: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "status: archived") {
		t.Errorf("expected status: archived in output, got:\n%s", text)
	}
	if !strings.Contains(text, "version: 9") {
		t.Errorf("expected version: 9 in output, got:\n%s", text)
	}
	if len(d.archiveCalls) != 1 {
		t.Errorf("archive dispatch count = %d, want 1", len(d.archiveCalls))
	}
}

func TestHandleMarkArchiveUnknownWorldSurfacesAsToolError(t *testing.T) {
	cfg := mcpTestConfig()
	pool := newWorldPool(cfg, fetch.Options{})
	g := newGatewayWithDispatcher(t, cfg, pool)
	res, err := g.handleMarkArchive(withAliceClaims(context.Background()), callToolReq("mark_archive", map[string]any{
		"url": "mark://team-b/foo.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkArchive: %v", err)
	}
	if !res.IsError {
		t.Fatal("isError = false on unknown world, want true")
	}
}

// TestWriteHandlersInheritPropagationRaceRetry uses an explicit
// fresh-mint 401 → ok scenario against publish to prove the
// retry logic carries through from dispatchWithAuth into writes.
// This is the load-bearing reason for the Slice 3 refactor —
// without dispatchWithAuth, writes would need their own retry
// loop and could silently regress.
func TestWriteHandlersInheritPropagationRaceRetry(t *testing.T) {
	cfg := mcpTestConfig()
	// Tight retry knobs so the test runs fast.
	cfg.Server.MCP.FirstMintMaxAttempts = 3
	cfg.Server.MCP.FirstMintInitialBackoff = 1
	cfg.Server.MCP.FirstMintMaxBackoff = 2

	var attempts int32
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			n := atomic.AddInt32(&attempts, 1)
			if n == 1 {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true after retry should have succeeded: %s", toolResultText(t, res))
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("publish attempts = %d, want 2 (one 401 then ok)", got)
	}
}

// TestMCPGatewayMarkPublishEndToEnd drives mark_publish through
// the full Streamable HTTP transport including gatewayAuth and
// session keying. The Slice 2 end-to-end test covers the read
// surface; this one pins the wire-shape contract for writes.
func TestMCPGatewayMarkPublishEndToEnd(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, body, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			if body != "## entry via gateway\n" {
				t.Errorf("dispatcher saw body=%q, want forwarded verbatim", body)
			}
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":  "1",
					"modified": "2026-05-21T11:00:00Z",
				},
			}}, nil
		},
	}
	verifier := &fakeVerifier{claims: Claims{
		Subject: "google|alice", Email: "alice@example.com", EmailVerified: true,
	}}
	signer := newTestSigner(t)
	issuer := newIssuer(t, cfg, fake.NewSimpleClientset())
	brokerSrv := NewServer(cfg, signer, verifier, issuer, nil, nil, nil)
	ts := httptest.NewServer(brokerSrv.MCPGatewayWith("test", d))
	t.Cleanup(ts.Close)

	initR := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if initR.HTTPStatus != http.StatusOK {
		t.Fatalf("initialize: status = %d", initR.HTTPStatus)
	}
	sessionID := initR.Headers[mcpSessionHeader]

	resp := mcpRequest(t, ts.URL, "alice-token", sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "mark_publish",
			"arguments": map[string]any{
				"url":              "mark://team-a/journal/2026-05-21.md",
				"body":             "## entry via gateway\n",
				"expected_version": float64(0),
			},
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
	for _, want := range []string{"status: ok", "version: 1", "modified: 2026-05-21T11:00:00Z"} {
		if !strings.Contains(text, want) {
			t.Errorf("end-to-end text missing %q\nfull:\n%s", want, text)
		}
	}
}
