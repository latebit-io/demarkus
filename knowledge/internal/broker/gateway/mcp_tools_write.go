package gateway

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// writeGrantFor is the grant claims earn on the world, or why they may not
// write to it. It runs before the dispatcher, so an unauthorized caller never
// reaches a world.
func (g *Gateway) writeGrantFor(claims *core.Claims, worldName string) (protocol.Grant, error) {
	worldCfg := core.LookupWorld(g.deps.Worlds, worldName)
	if worldCfg == nil {
		return protocol.Grant{}, fmt.Errorf("world %q is not configured", worldName)
	}
	return core.WriteGrant(worldCfg, claims)
}

// The write tools bind their arguments through mcpbind; marktools checks,
// authorizes through the Writer hook, and sends through docwrite.

func (g *Gateway) handleMarkPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	args, err := mcpbind.Publish(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Publish(ctx, args) })
}

func (g *Gateway) handleMarkAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Append(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Append(ctx, args) })
}

func (g *Gateway) handleMarkArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := mcpbind.URL(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Archive(ctx, raw) })
}
