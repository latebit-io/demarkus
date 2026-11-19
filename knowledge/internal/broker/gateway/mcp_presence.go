package gateway

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/presence"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/mark3labs/mcp-go/mcp"
)

// presenceMargin is how long past a bearer's life its holder stays listed:
// an idle library tab makes one request per bearer.
const presenceMargin = 5 * time.Minute

// PeerNotes carries one-line notes between the replicas, best effort: the
// local server's peer links. A request may reach any replica, so each tells
// the others whom it saw.
type PeerNotes interface {
	NotePeers(payload string) error
	// OnPeerNote names who takes the notes, nil for no one.
	OnPeerNote(take func(payload string))
}

// markPresent records the caller on its client and tells the peers when they
// are due. initialize is skipped: the session has no client name until it returns.
func (g *Gateway) markPresent(ctx context.Context, _ any, method mcp.MCPMethod, _ any) {
	if method == mcp.MethodInitialize {
		return
	}
	agent := mcpbind.ClientName(ctx)
	if agent == mcpbind.UnknownClient {
		agent = ""
	}
	entry, tell := g.present.Mark(toolUser(ctx), agent)
	if !tell || g.deps.Peers == nil {
		return
	}
	note, err := entry.Note()
	if err == nil {
		err = g.deps.Peers.NotePeers(note)
	}
	if err != nil {
		// The peers miss this sighting until the next is told.
		g.deps.Log.WarnContext(ctx, "broker: presence not told to peers", "err", err)
	}
}

// learnPresent takes a peer's note of whom it saw.
func (g *Gateway) learnPresent(payload string) {
	entry, err := presence.ParseNote(payload)
	if err != nil {
		g.deps.Log.Warn("broker: presence note rejected", "err", err)
		return
	}
	g.present.Learn(entry)
}

// clientsOf is the cell naming p's clients. A legacy session's client name
// lives only on the replica that opened it, until a restart or the idle
// sweep, so the unnamed client shows only for a person with no named one.
func clientsOf(p *presence.Person) string {
	named := slices.DeleteFunc(p.Agents, func(agent string) bool { return agent == "" })
	if len(named) == 0 {
		return mcpbind.UnknownClient
	}
	return strings.Join(named, ", ")
}

// handleMarkPresence lists who used the knowledge system within the window,
// on any replica. The caller is always among them: the call marks it.
func (g *Gateway) handleMarkPresence(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	people := g.present.People()
	var b strings.Builder
	fmt.Fprintf(&b, "status: ok\nwindow: %dm\ncount: %d\n", int(g.deps.PresenceWindow.Minutes()), len(people))
	if len(people) > 0 {
		b.WriteString("\n| user | clients | seen |\n|------|---------|------|\n")
		for i := range people {
			p := &people[i]
			fmt.Fprintf(&b, "| %s | %s | %s |\n", render.EscapeCell(p.User), render.EscapeCell(clientsOf(p)), p.Seen.UTC().Format(time.RFC3339))
		}
	}
	return mcpbind.Text(b.String()), nil
}
