package gateway

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/mark3labs/mcp-go/mcp"
)

// Memory-broker prompts: the plugin's /soul-* commands for plugin-less hosts,
// same names, bodies rendered from plugins/prompt-source (soul-export aside).
// Each addresses the caller's one world; tenantGate scopes every tool call.

//go:embed prompts/memory/commands/*.md
var memoryPromptFS embed.FS

// registerMemoryPrompts wires the memory prompts.
func (g *Gateway) registerMemoryPrompts() {
	prompts, err := mcpfmt.LoadPrompts(memoryPromptFS, "prompts/memory/commands")
	if err != nil {
		g.deps.Log.Error("mcp gateway: memory prompts unavailable", "err", err)
	}
	for _, p := range prompts {
		g.mcpServer.AddPrompt(p.Prompt, p.Handler)
	}
	g.mcpServer.AddPrompt(mcp.NewPrompt("soul-export",
		mcp.WithPromptDescription("Export your whole memory to local files: walk every document and save it, so the service is never a lock-in."),
		mcp.WithArgument("directory", mcp.ArgumentDescription("local directory to write the export into (default ./<prompt name>)")),
	), memoryExportPrompt)
}

func memoryExportPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	dir := strings.TrimSpace(req.Params.Arguments["directory"])
	if dir == "" {
		dir = "./" + req.Params.Name // "default ./<prompt name>", as the argument promises
	}
	return memoryExportResult(dir), nil
}

func memoryExportResult(dir string) *mcp.GetPromptResult {
	text := fmt.Sprintf(`Export this memory to local files under %q.

1. Call mark_worlds to learn your world name <w>.
2. Walk the tree: mark_list "mark://<w>/" and recurse into every subdirectory (follow next-cursor when a listing is incomplete; pass include_archived true so nothing is silently dropped).
3. For every document, mark_fetch with force=true and write the body verbatim to %s/<path>, preserving the directory layout. Record each document's version and modified timestamp in an export manifest %s/manifest.md as a table row.
4. Finish by reporting the document count and the manifest location. Do not summarize or rewrite bodies; this is a byte-preserving export.`, dir, dir, dir)
	return mcp.NewGetPromptResult("Soul export", []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	})
}
