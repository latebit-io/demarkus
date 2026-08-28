package broker

import "github.com/mark3labs/mcp-go/mcp"

// GatewayProfile selects which product the shared MCP gateway serves:
// the knowledge broker's full multi-world surface with org-open reads,
// or the memory broker's tenant-scoped soul surface.
type GatewayProfile struct {
	// ServerName is the MCP server name reported in the initialize
	// result (e.g. "demarkus-knowledge-broker").
	ServerName string
	// Instructions is the server-instructions text returned in the
	// initialize result. Hosts without a demarkus plugin (Claude
	// Desktop, ChatGPT) get their usage guidance from here.
	Instructions string
	// TenantScoped locks reads AND writes to the caller's single
	// world (tenantWorldFor). Cross-tenant access is denied at the
	// tool, resource, and crawl layers.
	TenantScoped bool
	// SeedSoul seeds the soul template into a tenant world on first
	// access (memory broker only).
	SeedSoul bool
	// Tools is the tool surface this profile registers.
	Tools []mcp.Tool
}

// KnowledgeGatewayProfile is the demarkus-knowledge-broker surface:
// all 17 tools, org-open reads, per-world Allow writes.
func KnowledgeGatewayProfile() *GatewayProfile {
	return &GatewayProfile{
		ServerName:   "demarkus-knowledge-broker",
		Instructions: knowledgeInstructions,
		Tools:        mcpTools(),
	}
}

// MemoryGatewayProfile is the demarkus-memory-broker surface: mark_*
// minus federation/multi-world (mark_lookup_all, mark_index, mark_resolve)
// and minus whole-store graph exports, which would leak cross-tenant edges.
func MemoryGatewayProfile() *GatewayProfile {
	return &GatewayProfile{
		ServerName:   "demarkus-memory-broker",
		Instructions: memoryInstructions,
		TenantScoped: true,
		SeedSoul:     true,
		Tools: []mcp.Tool{
			markFetchTool(),
			markExploreTool(),
			markListTool(),
			markVersionsTool(),
			markLookupTool(),
			markPublishTool(),
			markAppendTool(),
			markArchiveTool(),
			markDiscoverTool(),
			markBacklinksTool(),
			markGraphTool(),
			markWorldsSelfTool(),
		},
	}
}

// markWorldsSelfTool is the tenant-scoped mark_worlds variant: same
// output shape as the knowledge broker's, but the table only ever
// contains the caller's own world.
func markWorldsSelfTool() mcp.Tool {
	return mcp.NewTool("mark_worlds",
		mcp.WithDescription(
			"Show your own world: this memory service maps your identity to exactly "+
				"one private world, and every other tool addresses it as "+
				"mark://{worldName}/{path}. Returns a markdown table with columns: "+
				"world (your {worldName}), url (external address for a direct "+
				"client, may be blank), address (internal dial address), and "+
				"writable (always yes; your world is yours to write). Call this "+
				"once to learn your world name before using the other tools.",
		),
	)
}
