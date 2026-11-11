package main

import (
	"embed"
	"log"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// MCP prompts: workflows vended as commands (Claude Code: /mcp__<server>__<name>;
// Desktop: the prompt picker), so every connected agent runs the same flow.

// soulPromptFS holds the memory plugin's commands rendered for this surface.
//
//go:embed prompts/memory/commands/*.md
var soulPromptFS embed.FS

// workflow is the orient, recall and whats-new prompts for one store reached
// by bare paths: a lookup at "/" covers it.
func workflow(defaultHost string) *mcpfmt.Workflow {
	return &mcpfmt.Workflow{
		Document: "demarkus document",
		URLDesc:  urlDesc(defaultHost),

		RecallDesc:    "Recall what is known about a subject: catalog lookup first, then orient on the best match and read the relevant sections.",
		RecallExample: `"broker auth" or "release process"`,
		Store:         "demarkus server",
		Lookup:        `Call mark_lookup with url "/" and query %q; the catalog returns an importance-ranked table of matching documents. If nothing matches, retry once with a broader or synonymous query before concluding.`,
		Fetch:         `Call mark_fetch with url "<path>#<anchor>" for the sections that actually address the subject; avoid full bodies of large documents.`,
		Report:        `Report what is known, citing every claim with its mark:// path (and #anchor where applicable). If the catalog has nothing on the subject, say so plainly; never invent memory.`,

		WhatsNewDesc: "Digest of what changed on the server recently: catalog lookup filtered by modified date, newest first.",
		Where:        "on this demarkus server",
		Changed:      `Call mark_lookup with url "/", query "*", and filter "modified-after=<that date>"; it returns documents modified since then, importance-ranked, 10 unless limit is set.`,
		Read:         `For the most important changed documents, call mark_fetch with a #<anchor> section or rely on the outline to see what changed; do not fetch full bodies of large documents.`,
		Digest:       `Report a short digest, newest and most important first: each entry is the document (as a mark:// path), what it covers, and why it likely changed.`,
		Empty:        `If nothing changed, say so.`,
	}
}

// registerPrompts wires orient always. Recall, whats-new and the soul prompts
// look the -host store up by bare path, so they need one; the soul prompts
// also stay out where a plugin (lean profile) already ships them.
func registerPrompts(s *mcpserver.MCPServer, defaultHost, profile string) {
	prompts := workflow(defaultHost).Prompts()
	if defaultHost == "" {
		prompts = prompts[:1]
	}
	for _, p := range prompts {
		s.AddPrompt(p.Prompt, p.Handler)
	}
	if defaultHost == "" || profile == mcpfmt.ProfileLean {
		return
	}
	prompts, err := mcpfmt.LoadPrompts(soulPromptFS, "prompts/memory/commands")
	if err != nil {
		log.Printf("warning: soul prompts unavailable: %v", err)
	}
	for _, p := range prompts {
		s.AddPrompt(p.Prompt, p.Handler)
	}
}
