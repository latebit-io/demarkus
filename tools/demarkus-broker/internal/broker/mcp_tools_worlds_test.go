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

func TestHandleMarkWorldsListsAuthorizedOnly(t *testing.T) {
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
		"count: 1",
		"| world | url |",
		"| team-a | mark://team-a.example.org:6309 |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	// alice@example.com does not match secret-b's otherco.test allowlist:
	// the world must not leak into her universe.
	if strings.Contains(text, "secret-b") {
		t.Errorf("unauthorized world leaked: %q", text)
	}
}

func TestHandleMarkWorldsBlankPublicURL(t *testing.T) {
	// PublicURL is optional config; the worldName alone still addresses
	// the world through every tool, so the row renders with an empty cell
	// rather than being dropped.
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkWorlds(withAliceClaims(context.Background()), callToolReq("mark_worlds", nil))
	text := toolResultText(t, mustToolResult(t, res, err))
	if !strings.Contains(text, "| team-a |  |") {
		t.Errorf("blank-URL row missing: %q", text)
	}
}

func TestHandleMarkWorldsEmptyUniverse(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds[0].Allow = AllowConfig{Domains: []string{"otherco.test"}}
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
