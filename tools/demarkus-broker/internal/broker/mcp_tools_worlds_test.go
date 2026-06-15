package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// mustToolResult unwraps a successful tool call for the happy-path tests.
func mustToolResult(t *testing.T, res *mcp.CallToolResult, err error) *mcp.CallToolResult {
	t.Helper()
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", toolResultText(t, res))
	}
	return res
}

// worldsTestConfig is mcpTestConfig plus a second world with a
// different domain allowlist, so authorization filtering is visible.
func worldsTestConfig() *Config {
	cfg := mcpTestConfig()
	cfg.Worlds[0].PublicURL = "mark://team-a.example.org:6309"
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "secret-b",
		Namespace:    "secret-b",
		TokensSecret: "secret-b-tokens",
		Allow:        AllowConfig{Domains: []string{"otherco.test"}},
	})
	return cfg
}

func TestHandleMarkWorldsListsAllReadable(t *testing.T) {
	// Reads are gated by the broker SSO org gate alone, not the per-world
	// Allow (which is the WRITER allowlist). So mark_worlds lists EVERY
	// configured world to any authenticated identity — "secret-b" appears
	// even though alice does not match its writer Allow. Filtering the read
	// list by the writer allowlist is exactly the bug this corrects (it left
	// non-writers with an empty reading-room floor). Per-world READ
	// restrictions are a separate, not-yet-built predicate (Phase 2).
	g := newGatewayWithDispatcher(t, worldsTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkWorlds(withAliceClaims(context.Background()), callToolReq("mark_worlds", nil))
	if err != nil {
		t.Fatalf("handleMarkWorlds: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)

	for _, want := range []string{
		"status: ok",
		"count: 2",
		"| world | url | address |",
		// url = PublicURL; address = internal dial address (the topology graph's
		// node host) so the floor can join graph edges back to the worldName.
		"| team-a | mark://team-a.example.org:6309 | mark://team-a.team-a.svc.cluster.local:6309 |",
		// readable despite alice not matching its writer Allow.
		"secret-b",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
}

func TestHandleMarkWorldsBlankPublicURL(t *testing.T) {
	// PublicURL is optional config; the worldName alone still addresses
	// the world through every tool, so the url cell renders empty rather than
	// the row being dropped. The address cell is always populated (the
	// internal dial address the broker derives from Name + Namespace).
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkWorlds(withAliceClaims(context.Background()), callToolReq("mark_worlds", nil))
	text := toolResultText(t, mustToolResult(t, res, err))
	if !strings.Contains(text, "| team-a |  | mark://team-a.team-a.svc.cluster.local:6309 |") {
		t.Errorf("blank-URL row missing or address cell not populated: %q", text)
	}
}

func TestHandleMarkWorldsEmptyUniverse(t *testing.T) {
	// Only a genuinely world-less config yields an empty list now — a world's
	// writer Allow no longer hides it from the read list. Exercises the
	// count: 0 / no-table rendering branch.
	cfg := mcpTestConfig()
	cfg.Worlds = nil
	g := newGatewayWithDispatcher(t, cfg, &fakeDispatcher{})
	res, err := g.handleMarkWorlds(withAliceClaims(context.Background()), callToolReq("mark_worlds", nil))
	text := toolResultText(t, mustToolResult(t, res, err))
	if !strings.Contains(text, "count: 0") {
		t.Errorf("want count: 0, got %q", text)
	}
	if strings.Contains(text, "| world |") {
		t.Errorf("empty universe should render no table: %q", text)
	}
}

func TestHandleMarkWorldsMissingClaims(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkWorlds(context.Background(), callToolReq("mark_worlds", nil))
	if err != nil {
		t.Fatalf("handleMarkWorlds: %v", err)
	}
	if !res.IsError {
		t.Fatal("isError = false without identity, want tool error")
	}
}
