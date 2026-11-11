// Package toolname reads the tool names harnesses report for MCP calls and
// routes a call to the store it targets. The grammar is the harnesses'; the
// catalog it routes against is config's.
package toolname

import (
	"strings"

	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/toolvocab"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Parsed is the server and verb of an MCP tool name.
type Parsed struct {
	Server string // "" when un-prefixed (pi prefix "none")
	Verb   string // publish, append, fetch, ...
}

// StoreWrite reports a tool that changes a document store.
func (p Parsed) StoreWrite() bool { return toolvocab.StoreWrite[toolvocab.Prefix+p.Verb] }

// Parse reads mcp__<server>__mark_<verb> (Claude Code) and <server>_mark_<verb>
// or mark_<verb> (pi-mcp-adapter). The tool is the longest known name the
// string ends in, so a server named with "mark_" cannot shift the verb.
func Parse(tool string) (Parsed, bool) {
	name, server, ok := toolvocab.Suffix(tool)
	if !ok {
		return Parsed{}, false
	}
	server = strings.TrimPrefix(server, "mcp__")
	server = strings.TrimSuffix(server, "__")
	server = strings.TrimSuffix(server, "_")
	return Parsed{Server: server, Verb: strings.TrimPrefix(name, toolvocab.Prefix)}, true
}

// Qualify prefixes a bare tool name with its MCP server when the harness
// reports them separately (Cursor: tool_name + mcp_server_name), so Parse
// can resolve the target server. Already-qualified names pass through.
func Qualify(tool, server string) string {
	if server == "" || tool == "" {
		return tool
	}
	if pt, ok := Parse(tool); ok && pt.Server != "" {
		return tool
	}
	return "mcp__" + server + "__" + tool
}

// journalDir is the layout's journal directory as a path segment ("/journal/").
var journalDir = "/" + layout.Default().Journal

// IsJournalWrite reports a write that records a journal entry: the today route
// (marktools.RouteToday) or a path under the journal directory. The pi and
// OpenCode adapters mirror it.
func IsJournalWrite(tool, url, route string) bool {
	pt, ok := Parse(tool)
	if !ok || (pt.Verb != "publish" && pt.Verb != "append") {
		return false
	}
	return route == "today" || strings.Contains(url, journalDir)
}

func serverIsLocalMemory(server string) (bool, error) {
	if server == "" {
		return true, nil // un-prefixed: assume the local memory
	}
	return config.ResolvesToLocalMemory(server)
}

// MemoryTargetID is the canonical id of the memory a tool writes to (for the
// binding compare), or "" when it targets a knowledge system or another server.
func MemoryTargetID(tool string) (string, error) {
	pt, ok := Parse(tool)
	if !ok {
		return "", nil
	}
	local, err := serverIsLocalMemory(pt.Server)
	if err != nil {
		return "", err
	}
	if local {
		return config.LocalMemoryID, nil
	}
	remotes, err := config.ListRemoteMemories()
	if err != nil {
		return "", err
	}
	return named(remotes, pt.Server), nil
}

// KnowledgeScope is the registered knowledge-system slug a tool targets, or
// "". The local memory is never a knowledge target.
func KnowledgeScope(tool string) (string, error) {
	pt, ok := Parse(tool)
	if !ok {
		return "", nil
	}
	local, err := serverIsLocalMemory(pt.Server)
	if err != nil || local {
		return "", err
	}
	systems, err := config.ListKnowledgeSystems()
	if err != nil {
		return "", err
	}
	return named(systems, pt.Server), nil
}

// named is the slug server names, or "".
func named(slugs []string, server string) string {
	for _, slug := range slugs {
		if config.SameName(slug, server) {
			return slug
		}
	}
	return ""
}

// TargetsMemory reports a tool that targets a memory this plugin routes: the
// local managed memory or a registered remote one.
func TargetsMemory(tool string) (bool, error) {
	id, err := MemoryTargetID(tool)
	return id != "", err
}
