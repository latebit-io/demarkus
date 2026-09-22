package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/mark3labs/mcp-go/mcp"
)

// writeRefusal is why claims may not write to the world, nil when they may. It
// runs before the dispatcher, so an unauthorized caller never reaches a world.
// The email check repeats the bearer layer's on purpose (defense in depth).
func (g *Gateway) writeRefusal(claims *core.Claims, worldName string) error {
	if !claims.EmailVerified {
		return errors.New("identity email is not verified")
	}
	// A copy: the claims hang off a context concurrent tool calls share.
	canonical := *claims
	canonical.Email = core.CanonicalEmail(claims.Email)
	if canonical.Email == "" {
		return errors.New("identity has no email claim")
	}
	worldCfg := core.LookupWorld(g.deps.Worlds, worldName)
	if worldCfg == nil {
		return fmt.Errorf("world %q is not configured", worldName)
	}
	if !core.WorldAllows(&worldCfg.Allow, &canonical) {
		return fmt.Errorf("write access denied for world %q", worldName)
	}
	return nil
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
