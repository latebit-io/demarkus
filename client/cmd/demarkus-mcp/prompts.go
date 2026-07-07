package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// MCP prompts: the workflows the tool descriptions teach, vended as
// first-class commands (Claude Code surfaces them as /mcp__<server>__<name>
// slash commands; Desktop shows them in the prompt picker). The server
// versions the workflow — every connected agent gets the same flow without
// the model having to remember strategy prose.

// registerPrompts wires the three v1 prompts: orient, recall, whats-new.
func registerPrompts(s *mcpserver.MCPServer, defaultHost string) {
	s.AddPrompt(mcp.NewPrompt("orient",
		mcp.WithPromptDescription("Orient around a demarkus document: explore its neighborhood in one call, then read just the sections that matter."),
		mcp.WithArgument("url",
			mcp.RequiredArgument(),
			mcp.ArgumentDescription(urlDesc(defaultHost)),
		),
	), orientPrompt)

	s.AddPrompt(mcp.NewPrompt("recall",
		mcp.WithPromptDescription("Recall what is known about a subject: catalog lookup first, then orient on the best match and read the relevant sections."),
		mcp.WithArgument("subject",
			mcp.RequiredArgument(),
			mcp.ArgumentDescription("the subject to recall, e.g. \"broker auth\" or \"release process\""),
		),
	), recallPrompt)

	s.AddPrompt(mcp.NewPrompt("whats-new",
		mcp.WithPromptDescription("Digest of what changed on the server recently: catalog lookup filtered by modified date, newest first."),
		mcp.WithArgument("since",
			mcp.ArgumentDescription("start date as YYYY-MM-DD (default: 7 days ago)"),
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
	text := fmt.Sprintf(`Orient around the demarkus document %s.

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
	text := fmt.Sprintf(`Recall what this demarkus server knows about: %s.

1. Call mark_lookup with url "/" and query %q; the catalog returns an importance-ranked table of matching documents. If nothing matches, retry once with a broader or synonymous query before concluding.
2. Call mark_explore on the best match to see its outline and neighborhood; explore a second candidate only if the first does not cover the subject.
3. Call mark_fetch with url "<path>#<anchor>" for the sections that actually address the subject; avoid full bodies of large documents.
4. Report what is known, citing every claim with its mark:// path (and #anchor where applicable). If the catalog has nothing on the subject, say so plainly; never invent memory.`, subject, subject)
	return mcp.NewGetPromptResult("Recall: "+subject, []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}

func whatsNewPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	since := strings.TrimSpace(req.Params.Arguments["since"])
	sinceLine := `Use the date 7 days before today, formatted YYYY-MM-DD.`
	if since != "" {
		sinceLine = fmt.Sprintf("Use the date %q.", since)
	}
	text := fmt.Sprintf(`Summarize what changed on this demarkus server recently.

1. %s
2. Call mark_lookup with url "/", query "*", and filter "modified-after=<that date>"; this returns every catalogued document modified since then, importance-ranked.
3. For the handful of most important changed documents, call mark_fetch with a #<anchor> section or rely on the outline to see what changed; do not fetch full bodies of large documents.
4. Report a short digest, newest and most important first: each entry is the document (as a mark:// path), what it covers, and why it likely changed. If nothing changed, say so.`, sinceLine)
	title := "What's new"
	if since != "" {
		title = "What's new since " + since
	}
	return mcp.NewGetPromptResult(title, []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	}), nil
}
