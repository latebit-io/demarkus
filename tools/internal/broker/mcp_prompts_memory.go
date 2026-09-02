package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Memory-broker prompts: the memory workflows the demarkus-memory plugin
// ships as slash commands, restated for plugin-less hosts. Both address
// the caller's single world; tenantGate scopes every tool call they trigger.

// registerMemoryPrompts wires the three v1 memory prompts.
func (g *mcpGateway) registerMemoryPrompts() {
	g.mcpServer.AddPrompt(mcp.NewPrompt("memory-context",
		mcp.WithPromptDescription("Restore working context from your memory: the index hub, recent journal entries, and active plans, digested into a short brief."),
		mcp.WithArgument("focus",
			mcp.ArgumentDescription("optional subject to prioritize, e.g. a project or topic"),
		),
	), memoryContextPrompt)

	g.mcpServer.AddPrompt(mcp.NewPrompt("memory-journal",
		mcp.WithPromptDescription("Record a dated journal entry in your memory: append what happened this session to today's journal document."),
		mcp.WithArgument("entry",
			mcp.ArgumentDescription("optional summary of what to record; when omitted, distill the current conversation"),
		),
	), memoryJournalPrompt)

	g.mcpServer.AddPrompt(mcp.NewPrompt("memory-export",
		mcp.WithPromptDescription("Export your whole memory to local files: walk every document and save it, so the service is never a lock-in."),
		mcp.WithArgument("directory",
			mcp.ArgumentDescription("local directory to write the export into (default ./memory-export)"),
		),
	), memoryExportPrompt)

	// Pre-rename names stay registered for one release so hosts that saved
	// them keep working; same handlers, deprecated descriptions.
	g.mcpServer.AddPrompt(mcp.NewPrompt("soul-context",
		mcp.WithPromptDescription("Deprecated alias of memory-context."),
		mcp.WithArgument("focus", mcp.ArgumentDescription("optional subject to prioritize")),
	), memoryContextPrompt)
	g.mcpServer.AddPrompt(mcp.NewPrompt("soul-journal",
		mcp.WithPromptDescription("Deprecated alias of memory-journal."),
		mcp.WithArgument("entry", mcp.ArgumentDescription("optional summary of what to record")),
	), memoryJournalPrompt)
	g.mcpServer.AddPrompt(mcp.NewPrompt("soul-export",
		mcp.WithPromptDescription("Deprecated alias of memory-export."),
		mcp.WithArgument("directory", mcp.ArgumentDescription("local directory to write the export into")),
	), memoryExportPrompt)
}

func memoryExportPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	dir := strings.TrimSpace(req.Params.Arguments["directory"])
	if dir == "" {
		dir = "./memory-export"
	}
	text := fmt.Sprintf(`Export this memory to local files under %q.

1. Call mark_worlds to learn your world name <w>.
2. Walk the tree: mark_list "mark://<w>/" and recurse into every subdirectory (follow next-cursor when a listing is incomplete; pass include_archived true so nothing is silently dropped).
3. For every document, mark_fetch with force=true and write the body verbatim to %s/<path>, preserving the directory layout. Record each document's version and modified timestamp in an export manifest %s/manifest.md as a table row.
4. Finish by reporting the document count and the manifest location. Do not summarize or rewrite bodies; this is a byte-preserving export.`, dir, dir, dir)
	return mcp.NewGetPromptResult("Memory export", []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}

func memoryContextPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	focus := strings.TrimSpace(req.Params.Arguments["focus"])
	focusLine := ""
	if focus != "" {
		focusLine = fmt.Sprintf(" Prioritize anything related to: %s.", focus)
	}
	text := fmt.Sprintf(`Restore working context from this memory.%s

1. Call mark_worlds to learn your world name <w>; every URL below uses it.
2. Call mark_fetch with url "mark://<w>/index.md" to load the hub.
3. Call mark_list with url "mark://<w>/journal/" and read the newest one or two entries with mark_fetch.
4. If the hub lists active plans or open questions, mark_fetch the one or two most relevant documents (prefer #anchor section fetches for large ones).
5. Report a short brief: what was in progress, recent decisions, and anything flagged as next. Cite each claim as mark://<w>/<path>. If the memory is freshly seeded and has no history, say so plainly.`, focusLine)
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
2. Today's journal document is "mark://<w>/journal/<YYYY-MM-DD>.md" using today's date.
3. %s
4. Call mark_fetch on that URL. If it exists, mark_append a "## <short heading>" section with the entry. If it does not exist, mark_publish it with expected_version 0, a "# Journal <YYYY-MM-DD>" H1, and a metadata object with tags (subjects from the entry, plus "journal") and importance 0.4.
5. If the entry records a decision or a lesson worth finding later, also update the matching document (/adr/, /debugging.md, /patterns.md) and keep mark://<w>/index.md current.`, entryLine)
	return mcp.NewGetPromptResult("Memory journal", []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}
