// Package hosttools is the host tools the plugin binary provides to
// demarkus-mcp (client/mcpprovider): the plugin's state on the machine that
// runs the server, reachable where the harness shell is sandboxed. One table
// per tool is both the schema's enum and the dispatch; every action answers
// with the text the CLI subcommand prints, so prompts read one contract.
package hosttools

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/guidance"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/promote"
	"github.com/mark3labs/mcp-go/mcp"
)

// Tool names, classified in mcpfmt toolClasses: mark_host reads,
// mark_host_setup provisions and binds.
const (
	ReadTool  = "mark_host"
	SetupTool = "mark_host_setup"
)

// action is one enum value of a tool and what it runs.
type action struct {
	name string
	run  func(req mcp.CallToolRequest) (string, error)
}

var (
	readActions = []action{
		{"status", func(mcp.CallToolRequest) (string, error) { return provision.Status() }},
		{"auth", func(mcp.CallToolRequest) (string, error) { return provision.VerifyAuth() }},
		{"detect", func(mcp.CallToolRequest) (string, error) { return provision.DetectServers() }},
		{"project", withDir(Project)},
		{"catalog", withDir(Catalog)},
		{"promote", func(mcp.CallToolRequest) (string, error) {
			rows, err := promote.Detect(false)
			if err != nil {
				return "", err
			}
			return promote.Report(rows), nil
		}},
	}
	setupActions = []action{
		{"init", func(req mcp.CallToolRequest) (string, error) {
			mode, err := req.RequireString("mode")
			if err != nil {
				return "", err
			}
			return Init(mode, req.GetInt("port", 0), req.GetString("root", ""))
		}},
		{"default", func(req mcp.CallToolRequest) (string, error) {
			dir, err := req.RequireString("dir")
			if err != nil {
				return "", err
			}
			slug, err := req.RequireString("slug")
			if err != nil {
				return "", err
			}
			return Default(dir, slug)
		}},
	}
	tools = map[string][]action{ReadTool: readActions, SetupTool: setupActions}
)

// withDir runs fn on the required dir argument.
func withDir(fn func(dir string) (string, error)) func(mcp.CallToolRequest) (string, error) {
	return func(req mcp.CallToolRequest) (string, error) {
		dir, err := req.RequireString("dir")
		if err != nil {
			return "", err
		}
		return fn(dir)
	}
}

func names(actions []action) []string {
	out := make([]string, 0, len(actions))
	for i := range actions {
		out = append(out, actions[i].name)
	}
	return out
}

// Tools is the provided surface in registration order.
func Tools() []mcp.Tool {
	return []mcp.Tool{
		mcpfmt.NewTool(ReadTool,
			mcp.WithDescription("demarkus-memory plugin host state, read on the machine running this server (works from a sandboxed shell). "+
				"action: status (local soul mode, path, port, health, binaries); auth (token check); detect (reusable demarkus-servers); "+
				"project (dir: slug, store, root, verdict); catalog (dir: memories, default starred); promote (destinations). "+
				"Answers are the demarkus-plugin CLI text."),
			mcp.WithString("action", mcp.Required(), mcp.Enum(names(readActions)...)),
			mcp.WithString("dir", mcp.Description("project directory (project, catalog)")),
		),
		mcpfmt.NewTool(SetupTool,
			mcp.WithDescription("Change the demarkus-memory plugin's host setup. init: provision the local soul (mode default, isolated, "+
				"or reuse with port and root). default: bind dir's default memory to slug."),
			mcp.WithString("action", mcp.Required(), mcp.Enum(names(setupActions)...)),
			mcp.WithString("mode", mcp.Enum("default", "reuse", "isolated"), mcp.Description("init mode")),
			mcp.WithNumber("port", mcp.Description("reuse: server port")),
			mcp.WithString("root", mcp.Description("reuse: server root")),
			mcp.WithString("dir", mcp.Description("project directory (default)")),
			mcp.WithString("slug", mcp.Description("memory to bind (default)")),
		),
	}
}

// Call runs one host tool with its JSON arguments.
func Call(name string, args map[string]any) (string, error) {
	actions, ok := tools[name]
	if !ok {
		return "", fmt.Errorf("unknown host tool %q", name)
	}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	act, err := req.RequireString("action")
	if err != nil {
		return "", err
	}
	for i := range actions {
		if actions[i].name != act {
			continue
		}
		out, err := actions[i].run(req)
		if err != nil {
			return "", fmt.Errorf("%s: %w", act, err)
		}
		return out, nil
	}
	return "", fmt.Errorf("%s: unknown action %q", name, act)
}

// Project is `registry project`: the slug, store, root and verdict a prompt
// needs before its first mark_* call.
func Project(dir string) (string, error) {
	r, err := project.Resolve(dir)
	if err != nil {
		return "", err
	}
	st := guidance.Probe(r.Store, r.Slug)
	configured, err := guidance.LocalConfigured()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("slug=%s\nstore=%s\nroot=%s\nverdict=%s", r.Slug, r.Store, st.Root, guidance.Verdict(r, st, configured)), nil
}

// Catalog is `registry memory-default --list`: EMPTY or starred CATALOG
// rows, plus a trailing STALE line when the binding names a memory the
// catalog lacks, so callers stop instead of falling back to the local memory.
func Catalog(dir string) (string, error) {
	current, err := config.ProjectBinding(dir)
	if err != nil {
		return "", err
	}
	cat, err := catalog.Listing()
	if err != nil {
		return "", err
	}
	return strings.Join(DefaultListing(cat, current), "\n"), nil
}

// DefaultListing renders the catalog rows against the current binding.
func DefaultListing(cat []string, current string) []string {
	var out []string
	matched := false
	if len(cat) == 0 {
		out = append(out, "EMPTY")
	} else {
		out = append(out, "CATALOG")
		for _, row := range cat {
			marker := "-"
			if strings.SplitN(row, "\t", 2)[0] == current {
				marker = "*"
				matched = true
			}
			out = append(out, row+"\t"+marker)
		}
	}
	if current != "" && !matched {
		out = append(out, "STALE "+current)
	}
	return out
}

// Init is `provision init`: provisions the local soul in mode.
func Init(mode string, port int, root string) (string, error) {
	if err := provision.Init(mode, port, root); err != nil {
		return "", err
	}
	return "OK: provisioned (mode=" + mode + ")", nil
}

// Default is `registry memory-default --set`: binds dir's default memory.
func Default(dir, slug string) (string, error) {
	if err := catalog.BindProject(dir, slug); err != nil {
		return "", err
	}
	return "OK " + slug, nil
}
