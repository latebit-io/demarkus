package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// A session its client leaves without a DELETE, as Claude Code does, is
// unregistered once idle, so a long-lived pod does not keep every session;
// a client that comes back on the swept ID is still served.
func TestIdleSessionIsUnregisteredWithoutDelete(t *testing.T) {
	f := newGatewayFixture(t, mcpTestConfig(), &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, KnowledgeProfile())
	g := f.gateway(&fakeDispatcher{})
	// A one second TTL in place of production's; the fixture stops it.
	if err := g.transport.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.transport = g.newTransport(time.Second)
	ts := httptest.NewServer(g.Routes())
	t.Cleanup(ts.Close)

	initR := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	session := initR.Headers[mcpSessionHeader]
	if initR.HTTPStatus != http.StatusOK || session == "" {
		t.Fatalf("initialize: status %d, session %q", initR.HTTPStatus, session)
	}
	// DeleteSessionTools with no names reads the session registry without
	// touching the session's idle clock.
	if err := g.mcpServer.DeleteSessionTools(session); err != nil {
		t.Fatalf("session not registered after initialize: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(g.mcpServer.DeleteSessionTools(session), mcpserver.ErrSessionNotFound) {
		if time.Now().After(deadline) {
			t.Fatal("idle session still registered after 5s; nothing swept it")
		}
		time.Sleep(50 * time.Millisecond)
	}
	listR := mcpRequest(t, ts.URL, "alice-token", session, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	if listR.HTTPStatus != http.StatusOK {
		t.Errorf("tools/list on the swept session: status %d, body %s", listR.HTTPStatus, listR.RawBody)
	}
}
