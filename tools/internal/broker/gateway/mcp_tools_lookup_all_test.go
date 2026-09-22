package gateway

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestHandleMarkLookupAllMergesReadableWorlds(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds,
		core.WorldConfig{Name: "team-b", Namespace: "team-b"},
		core.WorldConfig{Name: "offline", Namespace: "offline"},
	)
	d := &fakeDispatcher{
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			world := r.Host
			switch world {
			case "team-a":
				return lookupResult(
					render.LookupRow{Path: "/auth|guide.md", Importance: 0.4, Title: "Auth [Guide]", Tags: []string{"auth", "guide"}},
					render.LookupRow{Path: "/architecture.md", Importance: 0.99, Title: "Architecture", Tags: []string{"design"}},
				), nil
			case "team-b":
				return lookupResult(
					render.LookupRow{Path: "/identity.md", Importance: 0.9, Title: "Identity", Tags: []string{"auth"}},
					render.LookupRow{Path: "/sessions.md", Importance: 0.2, Title: "Sessions", Tags: []string{"auth"}},
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

	if len(d.LookupCalls) != 3 {
		t.Fatalf("lookup dispatch count = %d, want 3", len(d.LookupCalls))
	}
	for _, call := range d.LookupCalls {
		if call.Scope != "/docs/" || call.Query != "auth" || call.Token != "" {
			t.Errorf("dispatch = %+v", call)
		}
		if call.Filter != "tag=auth" || call.Limit != 3 {
			t.Errorf("dispatch opts = %+v, want filter=tag=auth limit=3", call)
		}
	}
}

func TestHandleMarkLookupAllReportsTotalFailure(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "team-b", Namespace: "team-b"})
	d := &fakeDispatcher{
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			world := r.Host
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
		LookupFn: func(ctx context.Context, _ fetch.LookupRequest) (fetch.Result, error) {
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
		cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "team-" + strconv.Itoa(i), Namespace: "team"})
	}
	started := make(chan struct{}, len(cfg.Worlds))
	release := make(chan struct{})
	d := &fakeDispatcher{
		LookupFn: func(context.Context, fetch.LookupRequest) (fetch.Result, error) {
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
				LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
					got = r.Limit
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
	result := lookupResult(authRow)
	result.Response.Metadata["matches"] = "2"
	if _, err := parseLookupAllMatches("team-a", result); err == nil {
		t.Fatal("expected malformed response error")
	}
}

func TestParseLookupAllMatchesRejectsOutOfRangeImportance(t *testing.T) {
	result := lookupResult(authRow)
	result.Response.Body = strings.Replace(result.Response.Body, "| 0.90 |", "| 1.10 |", 1)
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

var authRow = render.LookupRow{Path: "/auth.md", Importance: 0.9, Title: "Auth", Tags: []string{"auth"}}

// lookupResult is the server's catalog answer for rows, match not carried.
func lookupResult(rows ...render.LookupRow) fetch.Result {
	return fetchtest.Lookup("q", "/", "", rows...)
}

// bodyLookupResult is the server's body match answer, echo included.
func bodyLookupResult(rows ...render.LookupRow) fetch.Result {
	return fetchtest.Lookup("q", "/", protocol.MatchBody, rows...)
}

func TestHandleMarkLookupAllRejectsUnknownMatchBeforeFanOut(t *testing.T) {
	d := &fakeDispatcher{}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	res, err := g.handleMarkLookupAll(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", map[string]any{
		"query": "hairpin", "match": "bogus",
	}))
	if err != nil || !res.IsError {
		t.Fatalf("err %v res %+v, want tool error", err, res)
	}
	if len(d.LookupCalls) != 0 {
		t.Errorf("dispatched %d lookups for an invalid match", len(d.LookupCalls))
	}
}

func TestHandleMarkLookupAllBodyMatchMergesAndFlagsCatalogWorlds(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "team-b", Namespace: "team-b"})
	var mu sync.Mutex
	var seen []fetch.LookupRequest
	d := &fakeDispatcher{
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			world := r.Host
			mu.Lock()
			seen = append(seen, r)
			mu.Unlock()
			if world == "team-a" {
				return bodyLookupResult(render.LookupRow{
					Path: "/debugging.md", Anchor: "hairpin-nat", Importance: 0.7,
					Title: "Debugging › Hairpin NAT", Tags: []string{"net"}, Snippet: "same host | hairpin",
				}), nil
			}
			// team-b predates body match: catalog answer, no echo.
			return lookupResult(render.LookupRow{Path: "/legacy.md", Importance: 0.9, Title: "Legacy", Tags: []string{"net"}}), nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	res, err := g.handleMarkLookupAll(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", map[string]any{
		"query": "hairpin", "match": "body",
	}))
	if err != nil || res.IsError {
		t.Fatalf("handleMarkLookupAll: err %v res %+v", err, res)
	}
	text := toolResultText(t, res)
	for _, want := range []string{
		"match: body",
		"| Path | Importance | Title | Tags | Snippet |",
		"| mark://team-a/debugging.md#hairpin-nat | 0.70 | Debugging › Hairpin NAT | net | same host \\| hairpin |",
		"| mark://team-b/legacy.md | 0.90 | Legacy | net |  |",
		"note: answered from the catalog (no body match): team-b",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, text)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, opts := range seen {
		if opts.Match != fetch.MatchBody {
			t.Errorf("dispatcher saw match %q, want body", opts.Match)
		}
	}
}

func TestHandleMarkLookupAllCapsTagsUnlessVerbose(t *testing.T) {
	cfg := mcpTestConfig()
	many := []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11", "t12"}
	d := &fakeDispatcher{
		LookupFn: func(context.Context, fetch.LookupRequest) (fetch.Result, error) {
			return lookupResult(render.LookupRow{Path: "/a.md", Importance: 0.9, Title: "A", Tags: many}), nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)
	call := func(args map[string]any) string {
		t.Helper()
		res, err := g.handleMarkLookupAll(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", args))
		if err != nil || res.IsError {
			t.Fatalf("handleMarkLookupAll: err %v res %+v", err, res)
		}
		return toolResultText(t, res)
	}
	lean := call(map[string]any{"query": "a"})
	if !strings.Contains(lean, "| t1, t2, t3, t4, t5, t6, t7, t8, t9, t10, +2 more |") {
		t.Errorf("lean rows not capped:\n%s", lean)
	}
	verbose := call(map[string]any{"query": "a", "verbose": true})
	if !strings.Contains(verbose, "| "+strings.Join(many, ", ")+" |") {
		t.Errorf("verbose rows capped:\n%s", verbose)
	}
}
