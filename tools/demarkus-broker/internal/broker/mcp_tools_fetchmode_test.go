package broker

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"k8s.io/client-go/kubernetes/fake"
)

const fetchModeDoc = "# Doc\n\nIntro paragraph.\n\n## Setup\n\nSetup body.\n\n## Usage\n\nUsage body.\n"

// bigFetchModeDoc returns a body over the outline threshold.
func bigFetchModeDoc() string {
	filler := strings.Repeat("filler line for section body padding\n", 150)
	return "# Big\n\nIntro paragraph.\n\n## Setup\n\n" + filler + "\n## Usage\n\n" + filler
}

// fetchModeDispatcher scripts one document with the given identity.
func fetchModeDispatcher(body, version, etag string) *fakeDispatcher {
	return &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": version, "modified": "2026-07-05T00:00:00Z", "etag": etag},
				Body:     body,
			}}, nil
		},
	}
}

// sessionCtx attaches an MCP client session with the given ID, matching
// what the Streamable HTTP transport installs on real traffic.
func sessionCtx(g *mcpGateway, sessionID string) context.Context {
	session := mcpserver.NewInProcessSession(sessionID, nil)
	return g.mcpServer.WithContext(withAliceClaims(context.Background()), session)
}

// fetchModeText runs mark_fetch through the gateway handler and returns
// the tool result text, failing the test on any error envelope.
func fetchModeText(ctx context.Context, t *testing.T, g *mcpGateway, args map[string]any) string {
	t.Helper()
	res, err := g.handleMarkFetch(ctx, callToolReq("mark_fetch", args))
	if err != nil {
		t.Fatalf("handleMarkFetch: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %+v", res.Content)
	}
	return toolResultText(t, res)
}

func TestHandleMarkFetchLargeDocOutline(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(bigFetchModeDoc(), "3", "abc"))
	text := fetchModeText(withAliceClaims(context.Background()), t, g, map[string]any{"url": "mark://team-a/big.md"})

	if !strings.Contains(text, "mode: outline") {
		t.Fatalf("large doc should return outline, got:\n%.200s", text)
	}
	if strings.Contains(text, "filler line") {
		t.Error("outline should not include section bodies")
	}
	for _, want := range []string{"#setup", "#usage", "Intro paragraph.", "fetch mark://team-a/big.md#<anchor> for a section", "size: "} {
		if !strings.Contains(text, want) {
			t.Errorf("outline missing %q", want)
		}
	}
}

func TestHandleMarkFetchForceReturnsFullBody(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(bigFetchModeDoc(), "3", "abc"))
	text := fetchModeText(withAliceClaims(context.Background()), t, g, map[string]any{"url": "mark://team-a/big.md", "force": true})
	if strings.Contains(text, "mode: outline") {
		t.Error("force=true should bypass outline mode")
	}
	if !strings.Contains(text, "filler line") {
		t.Error("force=true should return the full body")
	}
}

func TestHandleMarkFetchSectionSlice(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "3", "abc"))
	text := fetchModeText(withAliceClaims(context.Background()), t, g, map[string]any{"url": "mark://team-a/doc.md#setup"})
	for _, want := range []string{"section: #setup", "## Setup", "Setup body."} {
		if !strings.Contains(text, want) {
			t.Errorf("section slice missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Usage body.") {
		t.Error("section slice should not include other sections")
	}
}

func TestHandleMarkFetchSectionNotFoundListsAnchors(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "3", "abc"))
	res, err := g.handleMarkFetch(withAliceClaims(context.Background()), callToolReq("mark_fetch", map[string]any{
		"url": "mark://team-a/doc.md#nope",
	}))
	if err != nil {
		t.Fatalf("handleMarkFetch: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error for missing section")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "available anchors") || !strings.Contains(text, "setup") {
		t.Errorf("error should list available anchors, got: %s", text)
	}
}

func TestHandleMarkFetchSessionDedup(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "3", "abc"))
	ctx := sessionCtx(g, "session-1")
	url := map[string]any{"url": "mark://team-a/doc.md"}

	first := fetchModeText(ctx, t, g, url)
	if !strings.Contains(first, "Setup body.") {
		t.Fatal("first fetch should return full body")
	}

	second := fetchModeText(ctx, t, g, url)
	if !strings.Contains(second, "status: unchanged") || !strings.Contains(second, "unchanged since v3") {
		t.Fatalf("second fetch in same session should dedup, got:\n%s", second)
	}

	forced := fetchModeText(ctx, t, g, map[string]any{"url": "mark://team-a/doc.md", "force": true})
	if !strings.Contains(forced, "Setup body.") {
		t.Error("force=true should bust the dedup")
	}
}

func TestHandleMarkFetchDedupIsPerSession(t *testing.T) {
	// The broker is multi-tenant: one session's full-body fetch must
	// never produce an "unchanged" answer for a different session.
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "3", "abc"))
	url := map[string]any{"url": "mark://team-a/doc.md"}

	_ = fetchModeText(sessionCtx(g, "alice-session"), t, g, url)
	other := fetchModeText(sessionCtx(g, "bob-session"), t, g, url)
	if strings.Contains(other, "status: unchanged") {
		t.Fatal("dedup leaked across sessions")
	}
	if !strings.Contains(other, "Setup body.") {
		t.Error("other session should get the full body")
	}
}

func TestHandleMarkFetchNoSessionNoDedup(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "3", "abc"))
	ctx := withAliceClaims(context.Background()) // no MCP session attached
	url := map[string]any{"url": "mark://team-a/doc.md"}

	for range 2 {
		text := fetchModeText(ctx, t, g, url)
		if strings.Contains(text, "status: unchanged") {
			t.Fatal("sessionless fetch must never dedup")
		}
		if !strings.Contains(text, "Setup body.") {
			t.Error("sessionless fetch should return the body")
		}
	}
}

func TestHandleMarkFetchSessionEvictionDropsDedup(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "3", "abc"))
	ctx := sessionCtx(g, "ephemeral")
	url := map[string]any{"url": "mark://team-a/doc.md"}

	_ = fetchModeText(ctx, t, g, url)
	if _, ok := g.fetchSeen.lookup("ephemeral", "team-a/doc.md"); !ok {
		t.Fatal("full-body fetch should record dedup state")
	}
	g.fetchSeen.drop("ephemeral")
	text := fetchModeText(ctx, t, g, url)
	if strings.Contains(text, "status: unchanged") {
		t.Fatal("dropped session must not dedup")
	}
}

// capturedSessionSeen builds a sessionSeen with a log-capturing handler
// (so tests can assert the cap warnings fire) and a deterministic
// monotonic clock (each call to now advances one second).
func capturedSessionSeen() (*sessionSeen, *bytes.Buffer) {
	var logs bytes.Buffer
	tick := 0
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	s := newSessionSeen(slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	})
	return s, &logs
}

func TestSessionSeenCaps(t *testing.T) {
	t.Run("per-session doc cap stops recording and warns once", func(t *testing.T) {
		s, logs := capturedSessionSeen()
		for i := range maxSeenDocsPerSession + 10 {
			s.record("s1", fmt.Sprintf("w/doc-%d.md", i), seenDoc{version: "1"})
		}
		s.mu.Lock()
		n := len(s.byID["s1"].docs)
		s.mu.Unlock()
		if n != maxSeenDocsPerSession {
			t.Errorf("session doc count = %d, want cap %d", n, maxSeenDocsPerSession)
		}
		// Existing entries still update.
		s.record("s1", "w/doc-0.md", seenDoc{version: "2"})
		if d, _ := s.lookup("s1", "w/doc-0.md"); d.version != "2" {
			t.Error("existing entry should still update at cap")
		}
		// 10 rejected records, but the degradation warns exactly once.
		if got := strings.Count(logs.String(), "per-session doc cap reached"); got != 1 {
			t.Errorf("doc-cap warning logged %d times, want exactly 1\nlogs:\n%s", got, logs.String())
		}
	})
	t.Run("session cap evicts the least-recently-used session and warns", func(t *testing.T) {
		s, logs := capturedSessionSeen()
		for i := range maxSeenSessions {
			s.record(fmt.Sprintf("s%d", i), "w/doc.md", seenDoc{version: "1"})
		}
		// Touch s0 so it is no longer the oldest; s1 becomes the LRU.
		if _, ok := s.lookup("s0", "w/doc.md"); !ok {
			t.Fatal("s0 should be recorded")
		}
		if logs.Len() != 0 {
			t.Fatalf("no warning expected before the cap is exceeded, got:\n%s", logs.String())
		}
		s.record("one-over-cap", "w/doc.md", seenDoc{version: "1"})
		if _, ok := s.lookup("one-over-cap", "w/doc.md"); !ok {
			t.Error("new session past the cap should be recorded (evicting the LRU)")
		}
		if _, ok := s.lookup("s1", "w/doc.md"); ok {
			t.Error("least-recently-used session should have been evicted")
		}
		if _, ok := s.lookup("s0", "w/doc.md"); !ok {
			t.Error("recently-touched session must survive the eviction")
		}
		s.mu.Lock()
		n := len(s.byID)
		s.mu.Unlock()
		if n != maxSeenSessions {
			t.Errorf("session count = %d, want cap %d", n, maxSeenSessions)
		}
		if got := strings.Count(logs.String(), "session cap reached"); got != 1 {
			t.Errorf("eviction warning logged %d times, want exactly 1\nlogs:\n%s", got, logs.String())
		}
	})
}

// TestChangedNote pins the identity-delta wording, including the
// etag-only-identity edge (no version at all) that must not render a
// bare "still v".
func TestChangedNote(t *testing.T) {
	tests := []struct {
		name    string
		prev    seenDoc
		version string
		want    string
	}{
		{"version bump", seenDoc{version: "3"}, "5", "changed since this session's earlier fetch (v3 -> v5)"},
		{"was etag-only, now versioned", seenDoc{etag: "a"}, "5", "changed since this session's earlier fetch (was etag-only, now v5)"},
		{"was versioned, now etag-only", seenDoc{version: "3"}, "", "changed since this session's earlier fetch (was v3, now etag-only)"},
		{"etag rotated, version steady", seenDoc{version: "3", etag: "a"}, "3", "content changed since this session's earlier fetch (still v3, etag differs)"},
		{"etag-only identity", seenDoc{etag: "a"}, "", "content changed since this session's earlier fetch (etag differs)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := changedNote(tt.prev, tt.version); got != tt.want {
				t.Errorf("changedNote = %q, want %q", got, tt.want)
			}
		})
	}
}

// newTestMCPGatewayWith is newTestMCPGateway with an injected
// dispatcher, for HTTP-level tests that need scripted world responses.
func newTestMCPGatewayWith(t *testing.T, cfg *Config, verifier Verifier, d worldDispatcher) *httptest.Server {
	t.Helper()
	signer := newTestSigner(t)
	k8s := fake.NewSimpleClientset()
	brokerSrv := NewServer(cfg, signer, verifier, k8s, nil, nil, nil)
	ts := httptest.NewServer(brokerSrv.MCPGatewayWith("test", d))
	t.Cleanup(ts.Close)
	return ts
}

// TestMCPGatewayFetchDedupOverHTTP proves the session scoping works
// through the real Streamable HTTP transport, not just with a session
// hand-attached to the context: the Mcp-Session-Id issued at initialize
// is what keys the dedup, and a second initialize (new session) starts
// clean.
func TestMCPGatewayFetchDedupOverHTTP(t *testing.T) {
	v := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	ts := newTestMCPGatewayWith(t, mcpTestConfig(), v, fetchModeDispatcher(fetchModeDoc, "3", "abc"))

	initSession := func(id int) string {
		t.Helper()
		resp := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(id))
		if resp.HTTPStatus != http.StatusOK {
			t.Fatalf("initialize: status = %d, body = %s", resp.HTTPStatus, resp.RawBody)
		}
		sessionID := resp.Headers[mcpSessionHeader]
		if sessionID == "" {
			t.Fatalf("initialize response missing %s header", mcpSessionHeader)
		}
		return sessionID
	}
	fetchVia := func(id int, sessionID string) string {
		t.Helper()
		resp := mcpRequest(t, ts.URL, "alice-token", sessionID, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      "mark_fetch",
				"arguments": map[string]any{"url": "mark://team-a/doc.md"},
			},
		})
		if resp.HTTPStatus != http.StatusOK || resp.Error != nil {
			t.Fatalf("tools/call: status = %d, error = %+v, body = %s", resp.HTTPStatus, resp.Error, resp.RawBody)
		}
		content, ok := resp.Result["content"].([]any)
		if !ok || len(content) == 0 {
			t.Fatalf("tools/call result missing content: %+v", resp.Result)
		}
		entry, ok := content[0].(map[string]any)
		if !ok {
			t.Fatalf("content[0] not an object: %+v", content[0])
		}
		text, _ := entry["text"].(string)
		return text
	}

	session1 := initSession(1)
	first := fetchVia(2, session1)
	if !strings.Contains(first, "Setup body.") {
		t.Fatalf("first fetch should return the body, got:\n%s", first)
	}
	second := fetchVia(3, session1)
	if !strings.Contains(second, "status: unchanged") {
		t.Fatalf("second fetch in the same HTTP session should dedup, got:\n%s", second)
	}

	session2 := initSession(4)
	fresh := fetchVia(5, session2)
	if strings.Contains(fresh, "status: unchanged") {
		t.Fatal("dedup leaked across HTTP sessions")
	}
	if !strings.Contains(fresh, "Setup body.") {
		t.Error("new session should get the full body")
	}
}

func TestHandleMarkFetchNoIdentityNoDedup(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(fetchModeDoc, "", ""))
	ctx := sessionCtx(g, "session-1")
	url := map[string]any{"url": "mark://team-a/doc.md"}

	for range 2 {
		text := fetchModeText(ctx, t, g, url)
		if strings.Contains(text, "status: unchanged") {
			t.Fatal("fetch without version/etag must not dedup")
		}
	}
}
