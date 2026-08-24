package broker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestHandleMarkLookupAllMergesReadableWorlds(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds,
		WorldConfig{Name: "team-b", Namespace: "team-b"},
		WorldConfig{Name: "offline", Namespace: "offline"},
	)
	d := &fakeDispatcher{
		lookupFn: func(world, _, _, _ string, _ fetch.LookupOptions) (fetch.Result, error) {
			switch world {
			case "team-a":
				return lookupResult(
					"| /auth\\|guide.md | 0.40 | Auth \\[Guide\\] | auth, guide |",
					"| /architecture.md | 0.99 | Architecture | design |",
				), nil
			case "team-b":
				return lookupResult(
					"| /identity.md | 0.90 | Identity | auth |",
					"| /sessions.md | 0.20 | Sessions | auth |",
				), nil
			default:
				return fetch.Result{}, errors.New("dial timeout")
			}
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkLookupAll(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", map[string]any{
		"query":  "auth",
		"scope":  "/docs/",
		"filter": "tag=auth",
		"limit":  float64(3),
	}))
	if err != nil {
		t.Fatalf("handleMarkLookupAll: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %+v", res.Content)
	}
	text := toolResultText(t, res)
	for _, want := range []string{
		"status: partial",
		"worlds: 3",
		"succeeded: 2",
		"failed: 1",
		"matches: 3",
		"mark://team-a/auth%7Cguide.md",
		`Auth \[Guide\]`,
		"| offline | dial timeout |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}

	// Rank is comparable across worlds; importance only breaks ties within
	// the same rank. A second-ranked 0.99 result must follow both top hits.
	positions := []int{
		strings.Index(text, "mark://team-b/identity.md"),
		strings.Index(text, "mark://team-a/auth%7Cguide.md"),
		strings.Index(text, "mark://team-a/architecture.md"),
	}
	if positions[0] < 0 || positions[1] <= positions[0] || positions[2] <= positions[1] {
		t.Errorf("unexpected merged order: %v\n%s", positions, text)
	}
	if strings.Contains(text, "mark://team-b/sessions.md") {
		t.Errorf("global limit did not truncate fourth result:\n%s", text)
	}

	if len(d.lookupCalls) != 3 {
		t.Fatalf("lookup dispatch count = %d, want 3", len(d.lookupCalls))
	}
	for _, call := range d.lookupCalls {
		if call.scope != "/docs/" || call.query != "auth" || call.token != "" {
			t.Errorf("dispatch = %+v", call)
		}
		if call.opts.Filter != "tag=auth" || call.opts.Limit != 3 {
			t.Errorf("dispatch opts = %+v, want filter=tag=auth limit=3", call.opts)
		}
	}
}

func TestHandleMarkLookupAllReportsTotalFailure(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{Name: "team-b", Namespace: "team-b"})
	d := &fakeDispatcher{
		lookupFn: func(world, _, _, _ string, _ fetch.LookupOptions) (fetch.Result, error) {
			return fetch.Result{}, errors.New("unreachable " + world)
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkLookupAll(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", map[string]any{
		"query": "auth",
	}))
	if err != nil {
		t.Fatalf("handleMarkLookupAll: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error when every world fails")
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "team-a: unreachable team-a; team-b: unreachable team-b") {
		t.Errorf("failures are missing or nondeterministic:\n%s", text)
	}
}

func TestHandleMarkLookupAllRejectsInvalidArguments(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	tests := []struct {
		name string
		ctx  context.Context
		args map[string]any
	}{
		{name: "missing identity", ctx: context.Background(), args: map[string]any{"query": "auth"}},
		{name: "missing query", ctx: withAliceClaims(context.Background()), args: map[string]any{}},
		{name: "relative scope", ctx: withAliceClaims(context.Background()), args: map[string]any{"query": "auth", "scope": "docs/"}},
		{name: "control in scope", ctx: withAliceClaims(context.Background()), args: map[string]any{"query": "auth", "scope": "/docs/\nlimit: 1000"}},
		{name: "zero limit", ctx: withAliceClaims(context.Background()), args: map[string]any{"query": "auth", "limit": float64(0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := g.handleMarkLookupAll(tt.ctx, callToolReq("mark_lookup_all", tt.args))
			if err != nil {
				t.Fatalf("handleMarkLookupAll: %v", err)
			}
			if !res.IsError {
				t.Fatal("expected tool error")
			}
		})
	}
}

func TestHandleMarkLookupAllReturnsOnCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	d := &fakeDispatcher{
		lookupCtxFn: func(ctx context.Context, _, _, _, _ string, _ fetch.LookupOptions) (fetch.Result, error) {
			close(started)
			select {
			case <-release:
				return lookupResult(), nil
			case <-ctx.Done():
				return fetch.Result{}, ctx.Err()
			}
		},
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	ctx, cancel := context.WithCancel(withAliceClaims(context.Background()))
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := g.handleMarkLookupAll(ctx, callToolReq("mark_lookup_all", map[string]any{"query": "auth"}))
		done <- outcome{result: result, err: err}
	}()
	<-started
	cancel()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("handleMarkLookupAll: %v", got.err)
		}
		if !got.result.IsError {
			t.Fatal("expected canceled all-world lookup to return a tool error")
		}
	case <-time.After(time.Second):
		t.Fatal("lookup waited for blocked world after cancellation")
	}
	close(release)
}

func TestHandleMarkLookupAllBoundsFanout(t *testing.T) {
	cfg := mcpTestConfig()
	for i := 1; i < lookupAllWorkers+4; i++ {
		cfg.Worlds = append(cfg.Worlds, WorldConfig{Name: "team-" + strconv.Itoa(i), Namespace: "team"})
	}
	started := make(chan struct{}, len(cfg.Worlds))
	release := make(chan struct{})
	d := &fakeDispatcher{
		lookupFn: func(_, _, _, _ string, _ fetch.LookupOptions) (fetch.Result, error) {
			started <- struct{}{}
			<-release
			return lookupResult(), nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := g.handleMarkLookupAll(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", map[string]any{"query": "auth"}))
		done <- outcome{result: result, err: err}
	}()

	for range lookupAllWorkers {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("lookup workers did not start")
		}
	}
	select {
	case <-started:
		t.Fatalf("more than %d lookups ran concurrently", lookupAllWorkers)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("handleMarkLookupAll: %v", got.err)
		}
		if got.result.IsError {
			t.Fatalf("isError = true: %+v", got.result.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("lookup did not finish after workers were released")
	}
}

func TestHandleMarkLookupAllAppliesGlobalLimitBounds(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  map[string]any
		limit int
	}{
		{name: "default", args: map[string]any{"query": "auth"}, limit: defaultLookupAllLimit},
		{name: "cap", args: map[string]any{"query": "auth", "limit": float64(2000)}, limit: maxLookupAllResults},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got int
			d := &fakeDispatcher{
				lookupFn: func(_, _, _, _ string, opts fetch.LookupOptions) (fetch.Result, error) {
					got = opts.Limit
					return lookupResult(), nil
				},
			}
			g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
			result, err := g.handleMarkLookupAll(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", tt.args))
			if err != nil || result.IsError {
				t.Fatalf("handleMarkLookupAll: result=%+v err=%v", result, err)
			}
			if got != tt.limit {
				t.Errorf("world limit = %d, want %d", got, tt.limit)
			}
		})
	}
}

func TestParseLookupAllMatchesRejectsMetadataDrift(t *testing.T) {
	result := lookupResult("| /auth.md | 0.90 | Auth | auth |")
	result.Response.Metadata["matches"] = "2"
	if _, err := parseLookupAllMatches("team-a", result); err == nil {
		t.Fatal("expected malformed response error")
	}
}

func TestParseLookupAllMatchesRejectsOutOfRangeImportance(t *testing.T) {
	result := lookupResult("| /auth.md | 1.10 | Auth | auth |")
	if _, err := parseLookupAllMatches("team-a", result); err == nil {
		t.Fatal("expected malformed response error")
	}
}

func TestQualifiedLookupURLEncodesReservedPathBytes(t *testing.T) {
	got := qualifiedLookupURL("team-a", "/docs/a #?%.md")
	if want := "mark://team-a/docs/a%20%23%3F%25.md"; got != want {
		t.Errorf("qualifiedLookupURL = %q, want %q", got, want)
	}
}

func lookupResult(rows ...string) fetch.Result {
	body := "\n# Lookup matches\n\n| Path | Importance | Title | Tags |\n|------|------------|-------|------|\n"
	if len(rows) > 0 {
		body += strings.Join(rows, "\n") + "\n"
	}
	return fetch.Result{Response: protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"matches": strconv.Itoa(len(rows))},
		Body:     body,
	}}
}
