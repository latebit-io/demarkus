package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
)

// TestHandleMarkPublishHappyPath drives the simplest publish:
// known expected_version, default on_conflict, no validation
// errors. Pins the byte-for-byte proxy contract — body + version
// + modified pass through the broker unchanged.
func TestHandleMarkPublishHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		Published: map[string]fetch.Result{
			"team-a/foo.md": {Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "3"}}},
		},
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":        "4",
					"modified":       "2026-05-21T10:00:00Z",
					"server-version": "1",
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
	for _, want := range []string{"status: ok", "version: 4", "modified: 2026-05-21T10:00:00Z", "server-version: 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
	if len(d.PublishCalls) != 1 {
		t.Fatalf("publish dispatch count = %d, want 1", len(d.PublishCalls))
	}
	call := d.PublishCalls[0]
	if call.Host != "team-a" {
		t.Errorf("worldName = %q, want team-a", call.Host)
	}
	if call.Path != "/foo.md" {
		t.Errorf("path = %q, want /foo.md", call.Path)
	}
	if call.Body != "# new content\n" {
		t.Errorf("body = %q, want forwarded verbatim", call.Body)
	}
	if call.ExpectedVersion != 3 {
		t.Errorf("expectedVersion = %d, want 3", call.ExpectedVersion)
	}
	if call.Metadata["agent"] != "alice@example.com" {
		t.Errorf("publisher meta agent = %q, want canonical email", call.Metadata["agent"])
	}
}

func TestHandleMarkPublishForwardsMetadata(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "# new\n",
		"expected_version": float64(0),
		"on_conflict":      "fail",
		"metadata":         map[string]any{"tags": "go,auth", "importance": 0.9},
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	if len(d.PublishCalls) != 1 {
		t.Fatalf("publish dispatch count = %d, want 1", len(d.PublishCalls))
	}
	m := d.PublishCalls[0].Metadata
	if m["tags"] != "go,auth" {
		t.Errorf("forwarded tags = %q, want go,auth", m["tags"])
	}
	if m["importance"] != "0.9" {
		t.Errorf("forwarded importance = %q, want 0.9", m["importance"])
	}
	// Agent identity is applied last and cannot be spoofed by caller metadata.
	if m["agent"] != "alice@example.com" {
		t.Errorf("agent = %q, want alice@example.com", m["agent"])
	}
}

func TestHandleMarkPublishDeniesNonWriter(t *testing.T) {
	// Writer authorization happens at the broker BEFORE dispatch.
	// An SSO-authed identity whose email is not covered by the
	// world's Allow predicate gets a clear "write access denied"
	// tool error — the dispatcher is never invoked, no Secret is
	// touched. SSO is the org gate; WorldConfig.Allow is the
	// writer allowlist.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{}
	g := newGatewayWithDispatcher(t, cfg, d)
	ctx := core.CtxWithClaims(context.Background(), &core.Claims{
		Subject:       "google|carol",
		Email:         "carol@otherco.test", // not in example.com domain
		EmailVerified: true,
	})
	res, err := g.handleMarkPublish(ctx, callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "should not land\n",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if !res.IsError {
		t.Fatal("isError = false for non-writer identity, want true")
	}
	if text := toolResultText(t, res); !strings.Contains(text, "write access denied") {
		t.Errorf("tool error = %q, want it to name the writer-allow rejection", text)
	}
	if len(d.PublishCalls) != 0 {
		t.Errorf("dispatcher.Publish called %d times for denied write, want 0", len(d.PublishCalls))
	}
}

func TestHandleMarkPublishMergeGrantsOnlyThePublish(t *testing.T) {
	cfg := mcpTestConfig()
	var publishGrants []protocol.Grant
	var fetchTokens []string
	var mu sync.Mutex
	d := &fakeDispatcher{
		PublishFn: func(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			grant, _ := protocol.GrantFrom(ctx)
			mu.Lock()
			publishGrants = append(publishGrants, grant)
			mu.Unlock()
			if r.Token != "" {
				t.Errorf("publish carried token %q, want none", r.Token)
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"version": "4", "server-version": "4"},
			}}, nil
		},
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			token := r.Token
			mu.Lock()
			fetchTokens = append(fetchTokens, token)
			mu.Unlock()
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "4"},
				Body:     "world content\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	if _, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "my content\n",
		"expected_version": float64(2),
	})); err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(publishGrants) != 1 {
		t.Errorf("publish dispatched %d times, want 1", len(publishGrants))
	}
	if len(fetchTokens) != 2 {
		t.Errorf("fetch dispatched %d times, want 2 (base + current)", len(fetchTokens))
	}
	if len(publishGrants) == 1 && (publishGrants[0].Label != "alice@example.com" || !slices.Equal(publishGrants[0].Paths, []string{"/team-a/*"})) {
		t.Errorf("publish grant = %+v, want alice on the world's write scope", publishGrants[0])
	}
	for i, token := range fetchTokens {
		if token != "" {
			t.Errorf("merge fetch %d used token %q, want public read", i, token)
		}
	}
}

func TestFakeDispatcherSnapshotsMetaOnCapture(t *testing.T) {
	// The fake snapshots a write's metadata; a later change to the caller's
	// map must not rewrite the recorded call.
	d := &fakeDispatcher{}
	meta := map[string]string{"agent": "alice@example.com"}
	if _, err := d.Publish(t.Context(), fetch.WriteRequest{Host: "team-a", Path: "/foo.md", Body: "hello", Token: "token", Metadata: meta}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Caller mutates AFTER the call returns — simulating a
	// handler reusing one map across multiple dispatcher calls,
	// or a future code path that builds meta lazily.
	meta["agent"] = "mallory@evil.example"
	meta["injected"] = "should-not-appear"

	if got := d.PublishCalls[0].Metadata["agent"]; got != "alice@example.com" {
		t.Errorf("recorded meta[agent] = %q, want alice@example.com (mutation after Publish leaked into history)", got)
	}
	if _, ok := d.PublishCalls[0].Metadata["injected"]; ok {
		t.Error("recorded meta inherited a key added after Publish — snapshot broken")
	}
}

// TestHandleMarkAppendHappyPath drives an append with an
// explicit expected_version. Same byte-for-byte forwarding
// invariant as publish.
func TestHandleMarkAppendHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		AppendFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
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
	if len(d.AppendCalls) != 1 {
		t.Fatalf("append calls = %d, want 1", len(d.AppendCalls))
	}
	if got := d.AppendCalls[0].ExpectedVersion; got != 7 {
		t.Errorf("dispatcher saw expectedVersion=%d, want 7", got)
	}
	if got := d.AppendCalls[0].Body; got != "## new entry\n" {
		t.Errorf("dispatcher saw body=%q, want forwarded verbatim", got)
	}
}

func TestHandleMarkArchiveHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) {
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
	if len(d.ArchiveCalls) != 1 {
		t.Errorf("archive dispatch count = %d, want 1", len(d.ArchiveCalls))
	}
}

func TestHandleMarkArchiveUnknownWorldSurfacesAsToolError(t *testing.T) {
	cfg := mcpTestConfig()
	pool := NewWorldPool(cfg.Registry(), fetch.Options{})
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

func TestWriteContextFailsClosed(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	contexts := map[string]context.Context{
		"missing identity": context.Background(),
		"non-writer": core.CtxWithClaims(context.Background(), &core.Claims{
			Email:         "carol@otherco.test",
			EmailVerified: true,
		}),
	}
	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			granted, err := g.writeContext(ctx, "team-a")
			if !errors.Is(err, core.ErrNotAuthorized) {
				t.Fatalf("error = %v, want ErrNotAuthorized", err)
			}
			if granted != nil {
				t.Error("a context was granted despite failed authorization")
			}
		})
	}
}

// A refused world answer is the answer: nothing re-provisions or retries.
func TestWriteHandlersDoNotRetryARefusal(t *testing.T) {
	var attempts int32
	d := &fakeDispatcher{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			atomic.AddInt32(&attempts, 1)
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if text := toolResultText(t, res); !strings.Contains(text, protocol.StatusUnauthorized) {
		t.Fatalf("result = %q, want the world's refusal forwarded", text)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("publish attempts = %d, want 1", got)
	}
}

// TestMCPGatewayMarkPublishEndToEnd drives mark_publish through
// the full Streamable HTTP transport including gatewayAuth and
// session keying. The Slice 2 end-to-end test covers the read
// surface; this one pins the wire-shape contract for writes.
func TestMCPGatewayMarkPublishEndToEnd(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			body := r.Body
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
