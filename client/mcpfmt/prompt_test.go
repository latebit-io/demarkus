package mcpfmt

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
	"github.com/mark3labs/mcp-go/mcp"
)

const journalFile = "---\ndescription: \"Journal: append\"\nargument-hint: <entry text>\n---\n<!-- a -->\n<!-- b -->\n\nAppend.\n\nEntry: $ARGUMENTS\n"

func TestLoadPromptsServesEachFile(t *testing.T) {
	fsys := fstest.MapFS{
		"p/soul-journal.md": {Data: []byte(journalFile)},
		"p/soul-context.md": {Data: []byte("---\ndescription: Context\n---\nRead the primer.\n")},
	}
	prompts, err := LoadPrompts(fsys, "p")
	if err != nil || len(prompts) != 2 {
		t.Fatalf("LoadPrompts = %d prompts, %v", len(prompts), err)
	}
	byName := map[string]int{}
	for i, p := range prompts {
		byName[p.Prompt.Name] = i
	}
	journal := prompts[byName["soul-journal"]]
	if journal.Prompt.Description != "Journal: append" || len(journal.Prompt.Arguments) != 1 ||
		journal.Prompt.Arguments[0].Name != PromptArgument || journal.Prompt.Arguments[0].Description != "<entry text>" {
		t.Errorf("journal prompt = %+v, want the description and one argument described by the hint", journal.Prompt)
	}
	if got := mcpfmttest.PromptText(t, journal.Handler, map[string]string{PromptArgument: " shipped it "}); got != "Append.\n\nEntry: shipped it\n" {
		t.Errorf("journal body = %q, want markers stripped and the argument filled", got)
	}
	primer := prompts[byName["soul-context"]]
	if len(primer.Prompt.Arguments) != 0 {
		t.Errorf("a body without $ARGUMENTS takes no argument: %+v", primer.Prompt.Arguments)
	}
	if got := mcpfmttest.PromptText(t, primer.Handler, nil); got != "Read the primer.\n" {
		t.Errorf("context body = %q", got)
	}
	for _, c := range []struct {
		p    Prompt
		args map[string]string
	}{{journal, map[string]string{"entry": "shipped it"}}, {primer, map[string]string{PromptArgument: "x"}}} {
		req := mcp.GetPromptRequest{Params: mcp.GetPromptParams{Arguments: c.args}}
		if _, err := c.p.Handler(context.Background(), req); err == nil {
			t.Errorf("%s accepted undeclared arguments %v", c.p.Prompt.Name, c.args)
		}
	}
}

func TestLoadPromptsNamesEveryBrokenFile(t *testing.T) {
	fsys := fstest.MapFS{
		"p/ok.md":             {Data: []byte("---\ndescription: d\n---\nBody\n")},
		"p/no-frontmatter.md": {Data: []byte("# Body\n")},
		"p/unclosed.md":       {Data: []byte("---\ndescription: d\n# Body\n")},
		"p/no-description.md": {Data: []byte("---\nargument-hint: x\n---\nBody\n")},
		"p/bad-yaml.md":       {Data: []byte("---\ndescription: [\n---\nBody\n")},
		"p/empty.md":          {Data: []byte("---\ndescription: d\n---\n<!-- marker -->\n")},
	}
	prompts, err := LoadPrompts(fsys, "p")
	if len(prompts) != 1 || prompts[0].Prompt.Name != "ok" {
		t.Errorf("served %d prompts, want only ok", len(prompts))
	}
	for _, name := range []string{"no-frontmatter", "unclosed", "no-description", "bad-yaml", "empty"} {
		if err == nil || !strings.Contains(err.Error(), "p/"+name+".md") {
			t.Errorf("error %v does not name %s", err, name)
		}
	}
	if _, err := LoadPrompts(fstest.MapFS{}, "p"); err == nil {
		t.Error("an empty prompt directory must fail")
	}
}
