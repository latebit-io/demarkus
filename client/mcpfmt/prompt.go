package mcpfmt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	"gopkg.in/yaml.v3"
)

// ArgumentsToken is where a plugin host substitutes a command's arguments.
const ArgumentsToken = "$ARGUMENTS"

// PromptArgument is the one argument of a prompt whose body takes
// $ARGUMENTS, as a plugin command takes all its arguments as one string.
const PromptArgument = "arguments"

// Prompt is one generated command as an MCP prompt and its handler; a plain
// struct so mcpfmt stays free of mcp-go's server package.
type Prompt struct {
	Prompt  mcp.Prompt
	Handler func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error)
}

// LoadPrompts serves each command file tools/plugin-prompts generated into
// dir as the prompt of its file name. It returns the prompts that built and
// an error naming every file that did not.
func LoadPrompts(fsys fs.FS, dir string) ([]Prompt, error) {
	paths, err := fs.Glob(fsys, path.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no prompt files in %s", dir)
	}
	var prompts []Prompt
	var errs []error
	for _, p := range paths {
		prompt, err := loadPrompt(fsys, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("prompt %s: %w", p, err))
			continue
		}
		prompts = append(prompts, prompt)
	}
	return prompts, errors.Join(errs...)
}

func loadPrompt(fsys fs.FS, p string) (Prompt, error) {
	raw, err := fs.ReadFile(fsys, p)
	if err != nil {
		return Prompt{}, err
	}
	return buildPrompt(strings.TrimSuffix(path.Base(p), ".md"), raw)
}

// buildPrompt turns one generated command into a prompt: description and
// argument hint from its frontmatter, an argument exactly when the body
// holds $ARGUMENTS.
func buildPrompt(name string, raw []byte) (Prompt, error) {
	fm, err := protocol.SplitFrontmatter(raw)
	if err != nil {
		return Prompt{}, err
	}
	if !fm.Found {
		return Prompt{}, errors.New("no frontmatter")
	}
	var meta struct {
		Description  string `yaml:"description"`
		ArgumentHint string `yaml:"argument-hint"`
	}
	if err := yaml.Unmarshal(fm.Block, &meta); err != nil {
		return Prompt{}, fmt.Errorf("frontmatter: %w", err)
	}
	if strings.TrimSpace(meta.Description) == "" {
		return Prompt{}, errors.New("frontmatter has no description")
	}
	// string() copies the body alone, so the file buffer is not retained.
	body := TrimGeneratedHeader(string(fm.Body))
	if strings.TrimSpace(body) == "" {
		return Prompt{}, errors.New("empty body")
	}
	takesArg := strings.Contains(body, ArgumentsToken)
	opts := []mcp.PromptOption{mcp.WithPromptDescription(meta.Description)}
	if takesArg {
		opts = append(opts, mcp.WithArgument(PromptArgument, mcp.ArgumentDescription(meta.ArgumentHint)))
	}
	desc := meta.Description
	handler := func(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) { //nolint:gocritic // signature required by mcp-go
		// A stale client's renamed argument (the gateway's old entry) must not
		// read as blank: a blank journal entry distills the whole session.
		for k := range req.Params.Arguments {
			if k != PromptArgument || !takesArg {
				return nil, fmt.Errorf("prompt %s: unknown argument %q", name, k)
			}
		}
		text := body
		if takesArg {
			text = strings.ReplaceAll(body, ArgumentsToken, strings.TrimSpace(req.Params.Arguments[PromptArgument]))
		}
		return mcp.NewGetPromptResult(desc, []mcp.PromptMessage{
			mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
		}), nil
	}
	return Prompt{Prompt: mcp.NewPrompt(name, opts...), Handler: handler}, nil
}

// TrimGeneratedHeader drops the one-line HTML comments tools/plugin-prompts
// leads a file with (markdownlint directive, DO NOT EDIT marker) and the
// blank lines after them; a model has no use for either.
func TrimGeneratedHeader(text string) string {
	for {
		line, rest, found := strings.Cut(text, "\n")
		if !found || !strings.HasPrefix(line, "<!--") || !strings.HasSuffix(line, "-->") {
			return strings.TrimLeft(text, "\n")
		}
		text = rest
	}
}
