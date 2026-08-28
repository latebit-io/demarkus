package broker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"k8s.io/client-go/kubernetes/fake"
)

// TestHandleMarkPublishHappyPath drives the simplest publish:
// known expected_version, default on_conflict, no validation
// errors. Pins the byte-for-byte proxy contract — body + version
// + modified pass through the broker unchanged.
func TestHandleMarkPublishHappyPath(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		published: map[string]fetch.Result{
			"team-a/foo.md": {Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "3"}}},
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
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

func TestHandleMarkPublishForwardsMetadata(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
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
	if len(d.publishCalls) != 1 {
		t.Fatalf("publish dispatch count = %d, want 1", len(d.publishCalls))
	}
	m := d.publishCalls[0].meta
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
	ctx := ctxWithClaims(context.Background(), &Claims{
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
	if len(d.publishCalls) != 0 {
		t.Errorf("dispatcher.Publish called %d times for denied write, want 0", len(d.publishCalls))
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

// TestHandleMarkPublishMergeCleanOutcomeOK: publish succeeds
// without conflict on the first attempt. merge.Candidate
// returns OutcomeOK, and formatMergeOutcome delegates to
// formatToolResult so the success-path text is byte-for-byte
// identical to a plain on_conflict="fail" success. That's the
// load-bearing parity invariant: default-flip doesn't disturb
// the happy path's output shape.
func TestHandleMarkPublishMergeCleanOutcomeOK(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		published: map[string]fetch.Result{
			"team-a/foo.md": {Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "4"}}},
		},
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":        "5",
					"modified":       "2026-05-22T10:00:00Z",
					"server-version": "1",
				},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "# clean publish\n",
		"expected_version": float64(4),
		"on_conflict":      "merge",
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true on clean merge OK: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	for _, want := range []string{"status: ok", "version: 5", "modified: 2026-05-22T10:00:00Z", "server-version: 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("merge OK text missing %q\nfull:\n%s", want, text)
		}
	}
	if strings.Contains(text, "merge-candidate") {
		t.Errorf("clean merge produced merge-candidate envelope — should have been a plain OK: %s", text)
	}
}

// TestHandleMarkPublishMergeCandidateWithoutMarkers: the world
// reports conflict, the broker fetches base + current, Diff3
// produces a clean structural merge (no marker section). The
// agent gets the merge-candidate envelope with has-markers=false
// and the synthesized body.
func TestHandleMarkPublishMergeCandidateWithoutMarkers(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			// First publish: conflict.
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusConflict,
				Metadata: map[string]string{
					"version":        "5",
					"server-version": "5",
				},
			}}, nil
		},
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			switch path {
			case "/foo.md/v3":
				// Base: four-line document. Ours edits line 1,
				// theirs edits line 4 — different regions so
				// Diff3 produces a clean merge.
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"version": "3"},
					Body:     "line one\nline two\nline three\nline four\n",
				}}, nil
			case "/foo.md":
				// Current head: theirs edited line 4 only.
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"version": "5"},
					Body:     "line one\nline two\nline three\nTHEIRS edited line four\n",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url": "mark://team-a/foo.md",
		// Ours: edited line 1 only — disjoint from theirs.
		"body":             "OURS edited line one\nline two\nline three\nline four\n",
		"expected_version": float64(3),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true on merge candidate: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	for _, want := range []string{
		"status: merge-candidate",
		"your-version: 3",
		"current-version: 5",
		"publish-at-version: 5",
		"has-markers: false",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("candidate envelope missing %q\nfull:\n%s", want, text)
		}
	}
	// Both edits must survive in the merged body — that's the
	// proof Diff3 ran with the right base/ours/theirs triple.
	for _, want := range []string{"OURS edited line one", "THEIRS edited line four", "line two", "line three"} {
		if !strings.Contains(text, want) {
			t.Errorf("candidate body missing %q\nfull:\n%s", want, text)
		}
	}
}

// TestHandleMarkPublishMergeCandidateWithMarkers: both sides
// edited the SAME lines since the base. Diff3 emits git-style
// conflict markers (<<<<<<<, =======, >>>>>>>) and
// has-markers=true so the agent knows to inspect/resolve before
// republishing.
func TestHandleMarkPublishMergeCandidateWithMarkers(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"version": "5", "server-version": "5"},
			}}, nil
		},
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			switch path {
			case "/foo.md/v3":
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"version": "3"},
					Body:     "the original line\n",
				}}, nil
			case "/foo.md":
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"version": "5"},
					Body:     "their changed line\n",
				}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "my changed line\n",
		"expected_version": float64(3),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true on conflict-marker case: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "has-markers: true") {
		t.Errorf("expected has-markers: true for an overlapping-edit conflict; got:\n%s", text)
	}
	// Conflict markers — Diff3 emits the git-style triple.
	for _, want := range []string{"<<<<<<<", "=======", ">>>>>>>", "my changed line", "their changed line"} {
		if !strings.Contains(text, want) {
			t.Errorf("conflict-marker body missing %q\nfull:\n%s", want, text)
		}
	}
}

func TestHandleMarkPublishMergeUsesTokenOnlyForPublish(t *testing.T) {
	cfg := mcpTestConfig()
	var publishTokens, fetchTokens []string
	var mu sync.Mutex
	d := &fakeDispatcher{
		publishFn: func(_, _, _, token string, _ int, _ map[string]string) (fetch.Result, error) {
			mu.Lock()
			publishTokens = append(publishTokens, token)
			mu.Unlock()
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"version": "4", "server-version": "4"},
			}}, nil
		},
		fetchFn: func(_, _, token string) (fetch.Result, error) {
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
	if len(publishTokens) != 1 {
		t.Errorf("publish dispatched %d times, want 1", len(publishTokens))
	}
	if len(fetchTokens) != 2 {
		t.Errorf("fetch dispatched %d times, want 2 (base + current)", len(fetchTokens))
	}
	if len(publishTokens) == 1 && publishTokens[0] == "" {
		t.Error("publish dispatched without a publish token")
	}
	for i, token := range fetchTokens {
		if token != "" {
			t.Errorf("merge fetch %d used token %q, want public read", i, token)
		}
	}
}

// TestHandleMarkPublishDefaultOnConflictIsMerge: omit
// on_conflict entirely → broker takes the merge branch (not the
// "fail" branch, which was Slice 3's default). Drives the same
// conflict scenario as TestHandleMarkPublishFailConflictForwardsVerbatim
// but with NO explicit on_conflict and asserts the
// merge-candidate envelope appears instead of the verbatim
// conflict.
func TestHandleMarkPublishDefaultOnConflictIsMerge(t *testing.T) {
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"version": "3", "server-version": "3"},
			}}, nil
		},
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "3"},
				Body:     "world body\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "my body\n",
		"expected_version": float64(1),
		// on_conflict intentionally omitted
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true on default merge: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "status: merge-candidate") {
		t.Errorf("default on_conflict did not produce merge-candidate envelope — default may have regressed to \"fail\":\n%s", text)
	}
}

func TestHandleMarkPublishEmptyOnConflictNormalizesToMerge(t *testing.T) {
	// MCP clients send blank strings for optional fields
	// sometimes; the broker normalizes "" → "merge" rather than
	// erroring out. Pins the parity with the local
	// demarkus-mcp's surface.
	cfg := mcpTestConfig()
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"version": "2", "server-version": "2"},
			}}, nil
		},
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "2"},
				Body:     "x\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "y\n",
		"expected_version": float64(1),
		"on_conflict":      "   ",
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true on whitespace on_conflict: %s", toolResultText(t, res))
	}
	if !strings.Contains(toolResultText(t, res), "status: merge-candidate") {
		t.Errorf("whitespace on_conflict did not normalize to \"merge\":\n%s", toolResultText(t, res))
	}
}

// TestHandleMarkPublishMergeBaseVersionZero: expected_version=0
// means create-only. A conflict in that mode means the doc
// already exists; merge.Candidate falls through with base=""
// (no base fetch) and runs Diff3 against an empty base. Pinned
// by the local demarkus-mcp's contract — non-overlapping
// insertions on both sides go through cleanly.
func TestHandleMarkPublishMergeBaseVersionZero(t *testing.T) {
	cfg := mcpTestConfig()
	var versionedFetch atomic.Bool
	d := &fakeDispatcher{
		publishFn: func(_, _, _, _ string, _ int, _ map[string]string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"version": "1", "server-version": "1"},
			}}, nil
		},
		fetchFn: func(_, path, _ string) (fetch.Result, error) {
			// merge.Candidate with expected_version=0 must NOT
			// issue a FetchVersion — the local merge package's
			// contract says base="" in that case. A request to
			// any /vN path here would be a regression.
			if strings.Contains(path, "/v") {
				versionedFetch.Store(true)
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
				Body:     "head\n",
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "new\n",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true on create-only merge: %s", toolResultText(t, res))
	}
	if versionedFetch.Load() {
		t.Error("merge.Candidate fetched a versioned path with expected_version=0; should have used base=\"\"")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "your-version: 0") {
		t.Errorf("expected your-version: 0 in candidate envelope; got:\n%s", text)
	}
}

func TestHandleMarkPublishFailConflictForwardsVerbatim(t *testing.T) {
	// version-mismatch on the world surfaces as `conflict`
	// status. With on_conflict="fail" the broker forwards
	// verbatim — body + version + server-version unchanged —
	// so the agent can act on the conflict envelope the same
	// way against a brokered or direct-QUIC world. The
	// merge-candidate path (Slice 6) is the default; this test
	// pins the still-supported opt-out behavior.
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
		"on_conflict":      "fail",
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Errorf("isError = true on conflict with on_conflict=fail (must forward verbatim, not turn into a tool error): %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "status: conflict") {
		t.Errorf("expected status: conflict verbatim in output, got:\n%s", text)
	}
	if !strings.Contains(text, "server-version: 7") {
		t.Errorf("expected server-version: 7 in output, got:\n%s", text)
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
		versionsFn: func(_, _, token string) (fetch.Result, error) {
			atomic.AddInt32(&versionsHits, 1)
			if token != "" {
				t.Errorf("VERSIONS used token %q, want public read", token)
			}
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"total":   "12",
					"current": "12",
				},
			}}, nil
		},
		appendFn: func(_, _, _, token string, expectedVersion int, _ map[string]string) (fetch.Result, error) {
			atomic.AddInt32(&appendHits, 1)
			if token == "" {
				t.Error("APPEND dispatched without a publish token")
			}
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

func TestDispatchWithWriteAuthFailsClosed(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	contexts := map[string]context.Context{
		"missing identity": context.Background(),
		"non-writer": ctxWithClaims(context.Background(), &Claims{
			Email:         "carol@otherco.test",
			EmailVerified: true,
		}),
	}
	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			called := false
			_, err := g.dispatchWithWriteAuth(ctx, "team-a", func(string) (fetch.Result, error) {
				called = true
				return fetch.Result{}, nil
			})
			if !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("error = %v, want ErrNotAuthorized", err)
			}
			if called {
				t.Error("write operation reached dispatcher despite failed authorization")
			}
		})
	}
}

// TestWriteHandlersInheritPropagationRaceRetry pins publish-token
// propagation retry with an explicit 401 followed by success.
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

// TestWriteDispatchRecoversFromRotatedWorldSecret pins the 401 invalidation
// path: a cached token the world no longer accepts is re-provisioned
// mid-dispatch, so a rotated world Secret recovers within one call.
func TestWriteDispatchRecoversFromRotatedWorldSecret(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Server.MCP.FirstMintMaxAttempts = 3
	cfg.Server.MCP.FirstMintInitialBackoff = 1
	cfg.Server.MCP.FirstMintMaxBackoff = 2

	var attempts int32
	d := &fakeDispatcher{
		publishFn: func(_, _, _, token string, _ int, _ map[string]string) (fetch.Result, error) {
			atomic.AddInt32(&attempts, 1)
			if token == "stale-rotated-away" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
			}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	// Simulate the stale cache a rotation leaves behind: the pod still
	// holds a token the world no longer recognizes.
	g.srv.worldWriteTokens.mu.Lock()
	g.srv.worldWriteTokens.cache["team-a"] = "stale-rotated-away"
	g.srv.worldWriteTokens.mu.Unlock()

	res, err := g.handleMarkPublish(withAliceClaims(context.Background()), callToolReq("mark_publish", map[string]any{
		"url":              "mark://team-a/foo.md",
		"body":             "hello",
		"expected_version": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleMarkPublish: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true, want recovery after re-provision: %s", toolResultText(t, res))
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("publish attempts = %d, want 2 (stale 401, fresh ok)", got)
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
	k8s := fake.NewSimpleClientset()
	brokerSrv := NewServer(cfg, signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	ts := httptest.NewServer(brokerSrv.MCPGatewayWith("test", d, KnowledgeGatewayProfile()))
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
