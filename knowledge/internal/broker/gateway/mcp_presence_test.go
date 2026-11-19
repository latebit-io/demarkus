package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/presence"
)

// legacyProtocol still has sessions: the client names itself once, at
// initialize, and later requests carry only the session ID.
const legacyProtocol = mcp.LATEST_LEGACY_PROTOCOL_VERSION

// presentOn is who r lists, as "user on client, client".
func presentOn(r *watchReplica) []string {
	people := r.gateway.present.People()
	out := make([]string, 0, len(people))
	for i := range people {
		out = append(out, people[i].User+" on "+clientsOf(&people[i]))
	}
	return out
}

// A request marks its caller under the name the client gave at initialize;
// initialize itself marks no one, since the name is not on the session yet.
func TestARequestMarksItsCallerPresent(t *testing.T) {
	t.Parallel()
	for _, version := range []string{legacyProtocol, mcp.LATEST_PROTOCOL_VERSION} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			r := newWatchSystem(t).replica()
			c, _ := readerClient(t, r, "alice", version)
			if got := presentOn(r); slices.Contains(got, "alice@example.com on unknown") {
				t.Fatalf("present after the handshake = %v; want no unnamed client", got)
			}
			if _, err := c.ListTools(context.Background(), mcp.ListToolsRequest{}); err != nil {
				t.Fatal(err)
			}
			if got, want := presentOn(r), []string{"alice@example.com on watch-test"}; !slices.Equal(got, want) {
				t.Errorf("present = %v, want %v", got, want)
			}
		})
	}
}

// Replicas share no sessions: one serving a session another opened still
// lists the caller, whatever it knows of the client.
func TestAnotherReplicasSessionStillMarksItsCaller(t *testing.T) {
	t.Parallel()
	system := newWatchSystem(t)
	opened, other := system.replica(), system.replica()
	c, _ := readerClient(t, opened, "alice", legacyProtocol)
	resp := mcpRequest(t, other.url, "alice", c.GetSessionId(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if resp.HTTPStatus != http.StatusOK {
		t.Fatalf("status %d from the replica that did not open the session", resp.HTTPStatus)
	}
	people := other.gateway.present.People()
	if len(people) != 1 || people[0].User != "alice@example.com" {
		t.Errorf("present on the other replica = %+v, want alice", people)
	}
}

// peerEnd stands in for one replica's peer links: a note it sends reaches
// the taker of the end it is paired with.
type peerEnd struct {
	peer *peerEnd
	mu   sync.Mutex
	take func(string)
}

func peerPair() (a, b *peerEnd) {
	a, b = &peerEnd{}, &peerEnd{}
	a.peer, b.peer = b, a
	return a, b
}

func (e *peerEnd) taker() func(string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.take
}

func (e *peerEnd) NotePeers(payload string) error {
	if take := e.peer.taker(); take != nil {
		take(payload)
	}
	return nil
}

func (e *peerEnd) OnPeerNote(take func(string)) {
	e.mu.Lock()
	e.take = take
	e.mu.Unlock()
}

// A request reaches one replica, and all of them list its caller: each tells
// its peers whom it saw.
func TestReplicasShareWhoIsPresent(t *testing.T) {
	t.Parallel()
	system := newWatchSystem(t)
	a, b := peerPair()
	served, other := system.replicaWith(a), system.replicaWith(b)
	c, _ := readerClient(t, served, "alice", mcp.LATEST_PROTOCOL_VERSION)
	if _, err := c.ListTools(context.Background(), mcp.ListToolsRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"alice@example.com on watch-test"}
	if got := presentOn(served); !slices.Equal(got, want) {
		t.Errorf("present on the replica that served = %v, want %v", got, want)
	}
	if got := presentOn(other); !slices.Equal(got, want) {
		t.Errorf("present on its peer = %v, want %v", got, want)
	}

	// A gateway that shut down takes no more notes.
	if err := other.gateway.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.taker() != nil {
		t.Error("a gateway that shut down still takes its peers' notes")
	}
}

// A note that is not one is dropped, and the list stays as it was.
func TestAMalformedPresenceNoteIsDropped(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	r.gateway.learnPresent("not json")
	if got := presentOn(r); len(got) != 0 {
		t.Errorf("present = %v after a malformed note", got)
	}
}

// mark_presence answers with a row a person, the caller included, and the
// table's cells escaped.
func TestMarkPresenceListsPeople(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	r.gateway.present.Learn(presence.Entry{User: "zed|z@example.com", Agent: "demarkus-library", Seen: time.Now().Add(-time.Minute)})
	c, _ := readerClient(t, r, "alice", mcp.LATEST_PROTOCOL_VERSION)
	res, err := c.CallTool(context.Background(), callToolReq("mark_presence", nil))
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(t, res)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	window := fmt.Sprintf("window: %dm", int(r.gateway.deps.PresenceWindow.Minutes()))
	if res.IsError || len(lines) != 8 || !slices.Equal(lines[:6], []string{
		"status: ok", window, "count: 2", "",
		"| user | clients | seen |", "|------|---------|------|",
	}) {
		t.Fatalf("mark_presence =\n%s", text)
	}
	if !strings.HasPrefix(lines[6], "| alice@example.com | watch-test | 20") || !strings.HasPrefix(lines[7], `| zed\|z@example.com | demarkus-library | 20`) {
		t.Errorf("rows =\n%s\n%s", lines[6], lines[7])
	}
}

// A person's named clients show together; the unnamed one shows only for a
// person no replica has a name for.
func TestClientsOfFoldsTheUnnamedClient(t *testing.T) {
	t.Parallel()
	for want, agents := range map[string][]string{
		"claude-code, demarkus-library": {"", "claude-code", "demarkus-library"},
		"unknown":                       {""},
	} {
		if got := clientsOf(&presence.Person{Agents: agents}); got != want {
			t.Errorf("clientsOf(%q) = %q, want %q", agents, got, want)
		}
	}
}

// The window outlasts a bearer, at which an idle session makes its one
// request, whatever lifetime the broker is configured to mint.
func TestPresenceWindowFollowsTheBearerLifetime(t *testing.T) {
	t.Parallel()
	cfg := *newWatchSystem(t).cfg
	cfg.Server.IDTokenTTL = 40 * time.Minute
	f := gatewayFixtureFor(t, &cfg, core.SharedDeps{Verifier: watchVerifier()}, KnowledgeProfile())
	if got := DepsFor(&cfg, KnowledgeProfile(), f.shared, nil).PresenceWindow; got != 40*time.Minute+presenceMargin {
		t.Errorf("PresenceWindow = %s for a 40m bearer, want %s more", got, presenceMargin)
	}
}

// A tenant may learn of no one else, so a tenant-scoped gateway keeps no list
// and serves no tool for one.
func TestATenantScopedGatewayKeepsNoPresence(t *testing.T) {
	t.Parallel()
	r := memoryReplica(t)
	if r.gateway.present != nil {
		t.Error("the memory gateway holds a presence registry")
	}
	if slices.ContainsFunc(MemoryProfile().Tools, func(tool mcp.Tool) bool { return tool.Name == "mark_presence" }) {
		t.Error("the memory profile serves mark_presence")
	}
}
