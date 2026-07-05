package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// MCP prompts on the gateway: the knowledge-system workflows as
// first-class commands (Claude Code surfaces them as
// /mcp__<server>__<name> slash commands; Desktop shows a picker). These
// are deliberately NOT copies of the local demarkus-mcp prompts — a
// knowledge system spans worlds, so recall and whats-new start from
// mark_worlds and sweep per-world catalogs, which a single-world client
// never needs.

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
		mcp.WithPromptDescription("Digest of what changed across the knowledge system recently: per-world catalog lookups filtered by modified date, newest first."),
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

1. Call mark_explore with url %q — one call returns its outline (heading tree with #anchors), opening paragraph, outbound links, backlinks, and siblings.
2. From the outline, call mark_fetch with url "%s#<anchor>" for the one or two sections that matter to the current task. Do not fetch the full body of a large document unless a section fetch proves insufficient (then use force=true).
3. Answer with: what this document is (one sentence), where it sits (key links/backlinks), and what its most relevant sections say — citing each as %s#<anchor>.`, url, url, url, url)
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

1. Call mark_worlds to see the readable worlds.
2. For each world, call mark_lookup with url "mark://<world>/" and query %q — the catalog returns an importance-ranked table of matching documents. If nothing matches anywhere, retry once with a broader or synonymous query before concluding.
3. Call mark_explore on the best match to see its outline and neighborhood; explore a second candidate only if the first does not cover the subject.
4. Call mark_fetch with url "mark://<world>/<path>#<anchor>" for the sections that actually address the subject — avoid full bodies of large documents.
5. Report what is known, citing every claim with its mark://<world>/<path> (and #anchor where applicable). If no catalog has anything on the subject, say so plainly — never invent organizational memory.`, subject, subject)
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
	scopeLine := `Call mark_worlds, then for each readable world call mark_lookup with url "mark://<world>/", query "*", and filter "modified-after=<that date>".`
	if world != "" {
		scopeLine = fmt.Sprintf(`Call mark_lookup with url "mark://%s/", query "*", and filter "modified-after=<that date>".`, world)
	}
	text := fmt.Sprintf(`Summarize what changed in this knowledge system recently.

1. %s
2. %s Each lookup returns the world's catalogued documents modified since then, importance-ranked.
3. For the handful of most important changed documents, call mark_fetch with a #<anchor> section or rely on the outline — do not fetch full bodies of large documents.
4. Report a short digest, newest and most important first: each entry is the document (as mark://<world>/<path>), what it covers, and why it likely changed. If nothing changed, say so.`, sinceLine, scopeLine)

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
