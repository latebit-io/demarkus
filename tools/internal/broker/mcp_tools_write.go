package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/mark3labs/mcp-go/mcp"
)

// writeRefusal is why claims may not write to the world, nil when they may. It
// runs before the dispatcher, so an unauthorized caller never reaches a world.
// The email check repeats the bearer layer's on purpose (defense in depth).
func (g *mcpGateway) writeRefusal(claims *Claims, worldName string) error {
	if !claims.EmailVerified {
		return errors.New("identity email is not verified")
	}
	// A copy: the claims hang off a context concurrent tool calls share.
	canonical := *claims
	canonical.Email = canonicalEmail(claims.Email)
	if canonical.Email == "" {
		return errors.New("identity has no email claim")
	}
	worldCfg := lookupWorld(g.srv.cfg, worldName)
	if worldCfg == nil {
		return fmt.Errorf("world %q is not configured", worldName)
	}
	if !worldAllows(&worldCfg.Allow, &canonical) {
		return fmt.Errorf("write access denied for world %q", worldName)
	}
	return nil
}

// The write tools keep argument parsing here; marktools checks, authorizes
// through the Writer hook, and sends through docwrite.

func (g *mcpGateway) handleMarkPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	body, err := req.RequireString("body")
	if err != nil {
		return mcp.NewToolResultError("body is required"), nil
	}
	args := marktools.PublishArgs{URL: raw, Body: body, OnConflict: req.GetString("on_conflict", "")}
	// A missing or mistyped expected_version stays nil; the tool refuses it.
	if version, err := req.RequireInt("expected_version"); err == nil {
		args.ExpectedVersion = &version
	}
	if args.Metadata, err = marktools.MetadataArg(req.GetArguments()); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Publish(ctx, args) })
}

func (g *mcpGateway) handleMarkAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	body, err := req.RequireString("body")
	if err != nil {
		return mcp.NewToolResultError("body is required"), nil
	}
	args := marktools.AppendArgs{URL: raw, Body: body, ExpectedVersion: req.GetInt("expected_version", 0)}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Append(ctx, args) })
}

func (g *mcpGateway) handleMarkArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Archive(ctx, raw) })
}
