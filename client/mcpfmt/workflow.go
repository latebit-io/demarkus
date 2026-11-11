package mcpfmt

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Workflow is the orient, recall and whats-new prompts as one surface words
// them. The procedure is written once here; a surface supplies what it calls
// its store and how a lookup is scoped, so the two cannot drift apart.
type Workflow struct {
	Document string // what orient calls a document
	URLDesc  string // orient's url argument

	RecallDesc    string
	RecallExample string // example subjects, quoted
	Store         string // what recall calls the store
	Lookup        string // recall step 1; %q takes the subject
	Fetch         string // recall step 3
	Report        string // recall step 4

	WhatsNewDesc string
	Where        string // where whats-new looks
	Changed      string // whats-new step 2 over everything readable
	ChangedIn    string // step 2 for one world, %s takes it; "" on a surface without worlds
	Read         string // whats-new step 3
	Digest       string // whats-new step 4
	Empty        string // how an empty answer reads, appended to Digest
	EmptyIn      string // the same for one world
}

// Prompts is the three prompts in registration order: orient, recall,
// whats-new. Recall and whats-new look the store up, so a surface with no
// default store registers orient alone.
func (w *Workflow) Prompts() []Prompt {
	whatsNew := []mcp.PromptOption{
		mcp.WithPromptDescription(w.WhatsNewDesc),
		mcp.WithArgument("since", mcp.ArgumentDescription("start date as YYYY-MM-DD (default: 7 days ago)")),
	}
	if w.ChangedIn != "" {
		whatsNew = append(whatsNew, mcp.WithArgument("world", mcp.ArgumentDescription("restrict to one world (default: every readable world)")))
	}
	return []Prompt{
		{Prompt: mcp.NewPrompt("orient",
			mcp.WithPromptDescription("Orient around a "+w.Document+": explore its neighborhood in one call, then read just the sections that matter."),
			mcp.WithArgument("url", mcp.RequiredArgument(), mcp.ArgumentDescription(w.URLDesc)),
		), Handler: w.Orient},
		{Prompt: mcp.NewPrompt("recall",
			mcp.WithPromptDescription(w.RecallDesc),
			mcp.WithArgument("subject", mcp.RequiredArgument(), mcp.ArgumentDescription("the subject to recall, e.g. "+w.RecallExample)),
		), Handler: w.Recall},
		{Prompt: mcp.NewPrompt("whats-new", whatsNew...), Handler: w.WhatsNew},
	}
}

// requiredArg returns the trimmed argument or an error naming it: prompt
// arguments arrive as strings and empties are the common client bug.
func requiredArg(req *mcp.GetPromptRequest, name string) (string, error) {
	v := strings.TrimSpace(req.Params.Arguments[name])
	if v == "" {
		return "", fmt.Errorf("%s argument is required", name)
	}
	return v, nil
}

func promptResult(title, text string) *mcp.GetPromptResult {
	return mcp.NewGetPromptResult(title, []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	})
}

// Orient answers the orient prompt.
func (w *Workflow) Orient(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	url, err := requiredArg(&req, "url")
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf(`Orient around the %s %s.

1. Call mark_explore with url %q; one call returns its outline (heading tree with #anchors), opening paragraph, outbound links, backlinks, and siblings.
2. From the outline, call mark_fetch with url "%s#<anchor>" for the one or two sections that matter to the current task. Do not fetch the full body of a large document unless a section fetch proves insufficient (then use force=true).
3. Answer with: what this document is (one sentence), where it sits (key links/backlinks), and what its most relevant sections say, citing each as %s#<anchor>.`, w.Document, url, url, url, url)
	return promptResult("Orient around "+url, text), nil
}

// Recall answers the recall prompt.
func (w *Workflow) Recall(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	subject, err := requiredArg(&req, "subject")
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf(`Recall what this %s knows about: %s.

1. %s
2. Call mark_explore on the best match to see its outline and neighborhood; explore a second candidate only if the first does not cover the subject.
3. %s
4. %s`, w.Store, subject, fmt.Sprintf(w.Lookup, subject), w.Fetch, w.Report)
	return promptResult("Recall: "+subject, text), nil
}

// WhatsNew answers the whats-new prompt; world narrows it where the surface has worlds.
func (w *Workflow) WhatsNew(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
	since := strings.TrimSpace(req.Params.Arguments["since"])
	world := ""
	if w.ChangedIn != "" {
		world = strings.TrimSpace(req.Params.Arguments["world"])
	}
	sinceLine := "Use the date 7 days before today, formatted YYYY-MM-DD."
	if since != "" {
		sinceLine = fmt.Sprintf("Use the date %q.", since)
	}
	lookup, empty := w.Changed, w.Empty
	if world != "" {
		lookup, empty = fmt.Sprintf(w.ChangedIn, world), w.EmptyIn
	}
	text := fmt.Sprintf(`Summarize what changed %s recently.

1. %s
2. %s
3. %s
4. %s %s`, w.Where, sinceLine, lookup, w.Read, w.Digest, empty)

	title := "What's new"
	if since != "" {
		title += " since " + since
	}
	if world != "" {
		title += " in " + world
	}
	return promptResult(title, text), nil
}
