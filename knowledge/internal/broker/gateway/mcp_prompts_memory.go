package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Memory-broker prompts: the soul workflows the memory plugin ships as
// /soul-* slash commands, under the same names for plugin-less hosts. Each
// addresses the caller's single world; tenantGate scopes every tool call.

// registerMemoryPrompts wires the memory prompts.
func (g *Gateway) registerMemoryPrompts() {
	prompts := []struct {
		name, desc string
		arg        mcp.PromptOption
		handler    mcpserver.PromptHandlerFunc
	}{
		{"soul-context",
			"Restore working context from your memory: the index hub, recent journal entries, and active plans, digested into a short brief.",
			mcp.WithArgument("focus", mcp.ArgumentDescription("optional subject to prioritize, e.g. a project or topic")),
			memoryContextPrompt},
		{"soul-journal",
			"Record a dated journal entry in your memory: append what happened this session to today's journal document.",
			mcp.WithArgument("entry", mcp.ArgumentDescription("optional summary of what to record; when omitted, distill the current conversation")),
			memoryJournalPrompt},
		{"soul-export",
			"Export your whole memory to local files: walk every document and save it, so the service is never a lock-in.",
			mcp.WithArgument("directory", mcp.ArgumentDescription("local directory to write the export into (default ./<prompt name>)")),
			memoryExportPrompt},
	}
	for _, p := range prompts {
		g.mcpServer.AddPrompt(mcp.NewPrompt(p.name, mcp.WithPromptDescription(p.desc), p.arg), p.handler)
	}
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
	return mcp.NewGetPromptResult("Memory export", []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	})
}

func memoryContextPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	focus := strings.TrimSpace(req.Params.Arguments["focus"])
	focusLine := ""
	if focus != "" {
		focusLine = fmt.Sprintf(" Prioritize anything related to: %s.", focus)
	}
	text := fmt.Sprintf(`Restore working context from this memory.%s

1. Call mark_worlds to learn your world name <w>.
2. Call mark_fetch with url "mark://<w>/.context".
3. Report a short brief: what was in progress, recent decisions, and anything flagged as next. Cite each claim as mark://<w>/<path>; relay every Coverage line. If the memory is freshly seeded and has no history, say so plainly.`, focusLine)
	return mcp.NewGetPromptResult("Memory context", []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}

func memoryJournalPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	entry := strings.TrimSpace(req.Params.Arguments["entry"])
	entryLine := "Distill the current conversation into the entry: what was worked on, decisions made and why, and open threads. Capture the non-obvious; skip trivia."
	if entry != "" {
		entryLine = fmt.Sprintf("Record this entry (expand with relevant context from the conversation): %s", entry)
	}
	text := fmt.Sprintf(`Record a dated journal entry in this memory.

1. Call mark_worlds to learn your world name <w>.
2. %s
3. Call mark_append with url "mark://<w>/", route "today", body the entry, and tags naming its subjects (comma-separated).
4. Relay the status, path, version and every note from the result. A decision or lesson worth finding later also goes to its own document (/adr/, /debugging.md, /patterns.md); keep mark://<w>/index.md current.`, entryLine)
	return mcp.NewGetPromptResult("Memory journal", []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}
