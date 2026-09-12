package answerbench

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/tools/internal/retrievalbench"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

var readTools = map[string]bool{
	"mark_fetch": true, "mark_lookup": true, "mark_explore": true, "mark_list": true,
	"mark_backlinks": true, "mark_graph": true, "mark_versions": true, "mark_discover": true,
}

// ServeProxy preserves production schemas and handlers, restricting all URL
// arguments to the frozen endpoint. No answer keys are loaded by this process.
func ServeProxy(ctx context.Context, cfg retrievalbench.StdioConfig, host string) (err error) {
	backend, err := retrievalbench.OpenStdioSession(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, backend.Close()) }()
	tools, err := backend.ListTools(ctx)
	if err != nil {
		return err
	}
	server := mcpserver.NewMCPServer("frozen-demarkus", "1")
	for i := range tools {
		tool := &tools[i]
		if !readTools[tool.Name] {
			continue
		}
		if tool.Name == "mark_graph" && !strings.HasPrefix(host, "127.0.0.1:") {
			continue // A crawl could follow foreign authorities outside the frozen snapshot.
		}
		server.AddTool(*tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			raw, ok := args["url"].(string)
			if !ok {
				return mcp.NewToolResultError("url is required"), nil
			}
			loc, err := location(raw, host)
			if err != nil || strings.Contains(loc.Path, "..") || strings.Contains(loc.Path, "\\") {
				return mcp.NewToolResultError("URL outside frozen fixture scope"), nil
			}
			result, err := backend.Call(ctx, tool.Name, args)
			if err != nil {
				return nil, fmt.Errorf("fixture %s: %w", tool.Name, err)
			}
			if result.IsError {
				return mcp.NewToolResultError(result.Text), nil
			}
			return mcp.NewToolResultText(result.Text), nil
		})
	}
	return mcpserver.ServeStdio(server)
}
