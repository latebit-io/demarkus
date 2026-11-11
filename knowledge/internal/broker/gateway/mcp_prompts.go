package gateway

import "github.com/latebit-io/demarkus/client/mcpfmt"

// MCP prompts expose knowledge-system workflows. Unlike local prompts, recall
// and default whats-new aggregate every readable world.

// workflow is the orient, recall and whats-new prompts over every readable
// world; the procedure is mcpfmt's, shared with the stdio server.
var workflow = mcpfmt.Workflow{
	Document: "knowledge-system document",
	URLDesc:  mcpURLDesc,

	RecallDesc:    "Recall what the knowledge system knows about a subject: catalog lookup across every readable world, then orient on the best match.",
	RecallExample: `"deploy runbook" or "escrow rules"`,
	Store:         "knowledge system",
	Lookup:        `Call mark_lookup_all with query %q; it returns one globally limited table with qualified mark://<world>/<path> results across every readable world. If the tool call returns a top-level error envelope caused by aggregate, transport, authorization, or dispatch failure, disclose it and stop without claiming the catalog is empty. A status: partial response is not that error case: use successful matches but disclose failed worlds; a partial lookup with no matches is inconclusive. Retry once with a broader or synonymous query only when status is ok and the table is empty.`,
	Fetch:         `Call mark_fetch with the returned "mark://<world>/<path>#<anchor>" for sections that actually address the subject; avoid full bodies of large documents.`,
	Report:        `Report what is known, citing every claim with its mark://<world>/<path> (and #anchor where applicable). Say no catalog has anything on the subject only after a successful non-partial empty broader retry; never invent organizational memory.`,

	WhatsNewDesc: "Digest of what changed across the knowledge system recently: catalog lookup filtered by modified date and prioritized by relevance and importance.",
	Where:        "in this knowledge system",
	Changed:      `Call mark_lookup_all with query "*" and filter "modified-after=<that date>". If the tool call returns a top-level error envelope caused by aggregate, transport, authorization, or dispatch failure, disclose it and stop without claiming nothing changed. A status: partial response is not that error case: include successful results but disclose failed worlds.` + changedTail,
	ChangedIn:    `Call mark_lookup with url "mark://%s/", query "*", and filter "modified-after=<that date>". If the tool call returns a transport, authorization, or dispatch error, disclose it and stop without claiming nothing changed.` + changedTail,
	Read:         `For each selected document, call mark_explore first to obtain its outline and modified timestamp. Use the outline when sufficient; otherwise call mark_fetch with the selected document's complete mark://<world>/<path>#<anchor> URL, using an anchor returned by mark_explore. Do not fetch full bodies of large documents.`,
	Digest:       `Report a short digest of the selected candidates, newest first by modified timestamp: each entry is the document (as mark://<world>/<path>), what it covers, and why it likely changed. State that the limited lookup is not exhaustive.`,
	Empty:        `Only a successful non-partial empty lookup proves nothing changed; a partial lookup with no matches is inconclusive and must disclose the failed worlds.`,
	EmptyIn:      `Only a successful empty targeted lookup proves nothing changed.`,
}

const changedTail = ` The lookup returns catalogued documents modified since then, ordered by per-world relevance and importance.`

// The handlers, named for the tests that read their text.
var orientPrompt, recallPrompt, whatsNewPrompt = workflow.Orient, workflow.Recall, workflow.WhatsNew

// registerPrompts wires the profile's prompt surface: orient, recall,
// whats-new for the knowledge broker; the memory plugin's commands for the
// tenant-scoped memory broker (mcp_prompts_memory.go).
func (g *Gateway) registerPrompts() {
	if g.profile.TenantScoped {
		g.registerMemoryPrompts()
		return
	}
	for _, p := range workflow.Prompts() {
		g.mcpServer.AddPrompt(p.Prompt, p.Handler)
	}
}
