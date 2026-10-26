package gateway

import (
	"embed"

	"github.com/latebit-io/demarkus/client/mcpfmt"
)

// Memory-broker prompts: gateway-memory's manifest commands for plugin-less
// hosts, bodies rendered from plugins/prompt-source.
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
}
