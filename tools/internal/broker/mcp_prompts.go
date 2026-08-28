package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// MCP prompts expose knowledge-system workflows. Unlike local prompts, recall
// and default whats-new aggregate every readable world.

// registerPrompts wires the three v1 prompts: orient, recall, whats-new.
func (g *mcpGateway) registerPrompts() {
	g.mcpServer.AddPrompt(mcp.NewPrompt("orient",
		mcp.WithPromptDescription("Orient around a knowledge-system document: explore its neighborhood in one call, then read just the sections that matter."),
		mcp.WithArgument("url",
			mcp.RequiredArgument(),
			mcp.ArgumentDescription(mcpURLDesc),
		),
	), orientPrompt)

	g.mcpServer.AddPrompt(mcp.NewPrompt("recall",
		mcp.WithPromptDescription("Recall what the knowledge system knows about a subject: catalog lookup across every readable world, then orient on the best match."),
		mcp.WithArgument("subject",
			mcp.RequiredArgument(),
			mcp.ArgumentDescription("the subject to recall, e.g. \"deploy runbook\" or \"escrow rules\""),
		),
	), recallPrompt)

	g.mcpServer.AddPrompt(mcp.NewPrompt("whats-new",
		mcp.WithPromptDescription("Digest of what changed across the knowledge system recently: catalog lookup filtered by modified date and prioritized by relevance and importance."),
		mcp.WithArgument("since",
			mcp.ArgumentDescription("start date as YYYY-MM-DD (default: 7 days ago)"),
		),
		mcp.WithArgument("world",
			mcp.ArgumentDescription("restrict to one world (default: every readable world)"),
		),
	), whatsNewPrompt)
}

// requiredArg returns the trimmed argument value or an error naming it —
// prompt arguments arrive as strings and empties are the common client bug.
func requiredArg(req *mcp.GetPromptRequest, name string) (string, error) {
	v := strings.TrimSpace(req.Params.Arguments[name])
	if v == "" {
		return "", fmt.Errorf("%s argument is required", name)
	}
	return v, nil
}

func orientPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	url, err := requiredArg(&req, "url")
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf(`Orient around the knowledge-system document %s.

1. Call mark_explore with url %q; one call returns its outline (heading tree with #anchors), opening paragraph, outbound links, backlinks, and siblings.
2. From the outline, call mark_fetch with url "%s#<anchor>" for the one or two sections that matter to the current task. Do not fetch the full body of a large document unless a section fetch proves insufficient (then use force=true).
3. Answer with: what this document is (one sentence), where it sits (key links/backlinks), and what its most relevant sections say, citing each as %s#<anchor>.`, url, url, url, url)
	return mcp.NewGetPromptResult("Orient around "+url, []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}

func recallPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	subject, err := requiredArg(&req, "subject")
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf(`Recall what this knowledge system knows about: %s.

1. Call mark_lookup_all with query %q; it returns one globally limited table with qualified mark://<world>/<path> results across every readable world. If the tool call returns a top-level error envelope caused by aggregate, transport, authorization, or dispatch failure, disclose it and stop without claiming the catalog is empty. A status: partial response is not that error case: use successful matches but disclose failed worlds; a partial lookup with no matches is inconclusive. Retry once with a broader or synonymous query only when status is ok and the table is empty.
2. Call mark_explore on the best match to see its outline and neighborhood; explore a second candidate only if the first does not cover the subject.
3. Call mark_fetch with the returned "mark://<world>/<path>#<anchor>" for sections that actually address the subject; avoid full bodies of large documents.
4. Report what is known, citing every claim with its mark://<world>/<path> (and #anchor where applicable). Say no catalog has anything on the subject only after a successful non-partial empty broader retry; never invent organizational memory.`, subject, subject)
	return mcp.NewGetPromptResult("Recall: "+subject, []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}

func whatsNewPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	since := strings.TrimSpace(req.Params.Arguments["since"])
	world := strings.TrimSpace(req.Params.Arguments["world"])

	sinceLine := "Use the date 7 days before today, formatted YYYY-MM-DD."
	if since != "" {
		sinceLine = fmt.Sprintf("Use the date %q.", since)
	}
	scopeLine := `Call mark_lookup_all with query "*" and filter "modified-after=<that date>". If the tool call returns a top-level error envelope caused by aggregate, transport, authorization, or dispatch failure, disclose it and stop without claiming nothing changed. A status: partial response is not that error case: include successful results but disclose failed worlds.`
	conclusionLine := `Only a successful non-partial empty lookup proves nothing changed; a partial lookup with no matches is inconclusive and must disclose the failed worlds.`
	if world != "" {
		scopeLine = fmt.Sprintf(`Call mark_lookup with url "mark://%s/", query "*", and filter "modified-after=<that date>". If the tool call returns a transport, authorization, or dispatch error, disclose it and stop without claiming nothing changed.`, world)
		conclusionLine = `Only a successful empty targeted lookup proves nothing changed.`
	}
	text := fmt.Sprintf(`Summarize what changed in this knowledge system recently.

1. %s
2. %s The lookup returns catalogued documents modified since then, ordered by per-world relevance and importance.
3. For each selected document, call mark_explore first to obtain its outline and modified timestamp. Use the outline when sufficient; otherwise call mark_fetch with the selected document's complete mark://<world>/<path>#<anchor> URL, using an anchor returned by mark_explore. Do not fetch full bodies of large documents.
4. Report a short digest of the selected candidates, newest first by modified timestamp: each entry is the document (as mark://<world>/<path>), what it covers, and why it likely changed. State that the limited lookup is not exhaustive. %s`, sinceLine, scopeLine, conclusionLine)

	title := "What's new"
	if since != "" {
		title += " since " + since
	}
	if world != "" {
		title += " in " + world
	}
	return mcp.NewGetPromptResult(title, []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}
