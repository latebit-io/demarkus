// Package config reads the shared ~/.demarkus plugin state — the single source
// of truth that every demarkus plugin (Claude Code, pi, Codex, …) consults via
// the demarkus-plugin binary. Readers fail open ONLY on "file absent" (ENOENT);
// any other I/O error propagates so a present-but-unreadable config can't
// silently disable a gate.
package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Strictness is the enforcement level a gate applies to a violation.
type Strictness string

const (
	Warn  Strictness = "warn"
	Block Strictness = "block"
	Ask   Strictness = "ask"
)

// LocalSoulID is the reserved catalog id / MCP server name of the local managed soul.
const LocalSoulID = "demarkus-memory"

// Paths under ~/.demarkus. Resolved once via Home().
func Home() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".demarkus"), nil
}

func path(name string) (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, name), nil
}

// readRaw returns a file's contents, treating only "does not exist" as empty.
// Any other error (permissions, I/O) propagates — a present-but-unreadable
// config/registry must not look absent and silently turn a gate off.
func readRaw(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

func readTrimmed(p string) (string, error) {
	s, err := readRaw(p)
	return strings.TrimSpace(s), err
}

// records returns non-blank, non-comment, trimmed lines.
func records(p string) ([]string, error) {
	s, err := readRaw(p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		out = append(out, ln)
	}
	return out, nil
}

func fileExists(name string) (bool, error) {
	p, err := path(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// --- strictness ---------------------------------------------------------------

func validStrictness(s string, def Strictness) Strictness {
	switch Strictness(strings.TrimSpace(s)) {
	case Warn, Block, Ask:
		return Strictness(strings.TrimSpace(s))
	default:
		return def
	}
}

// MemoryTagStrictness: env override → plugin-memory.strictness → warn (floor).
func MemoryTagStrictness() (Strictness, error) {
	if v := os.Getenv("DEMARKUS_MEMORY_STRICTNESS"); v != "" {
		return validStrictness(v, Warn), nil
	}
	p, err := path("plugin-memory.strictness")
	if err != nil {
		return Warn, err
	}
	s, err := readTrimmed(p)
	if err != nil {
		return Warn, err
	}
	return validStrictness(s, Warn), nil
}

// MemoryDestStrictness: env override → plugin-memory.dest-strictness → block.
func MemoryDestStrictness() (Strictness, error) {
	if v := os.Getenv("DEMARKUS_MEMORY_DEST_STRICTNESS"); v != "" {
		return validStrictness(v, Block), nil
	}
	p, err := path("plugin-memory.dest-strictness")
	if err != nil {
		return Block, err
	}
	s, err := readTrimmed(p)
	if err != nil {
		return Block, err
	}
	return validStrictness(s, Block), nil
}

// KnowledgeStrictness: env override → plugin-knowledge.strictness.<slug> → warn.
func KnowledgeStrictness(slug string) (Strictness, error) {
	if v := os.Getenv("DEMARKUS_KNOWLEDGE_STRICTNESS"); v != "" {
		return validStrictness(v, Warn), nil
	}
	if slug == "" {
		return Warn, nil
	}
	p, err := path("plugin-knowledge.strictness." + slug)
	if err != nil {
		return Warn, err
	}
	s, err := readTrimmed(p)
	if err != nil {
		return Warn, err
	}
	return validStrictness(s, Warn), nil
}

// tokens splits a comma/whitespace/newline-separated list into clean tokens.
func tokens(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	var out []string
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// KnowledgeRequireTags: per-slug required tag axes (e.g. "category team").
func KnowledgeRequireTags(slug string) ([]string, error) {
	if slug == "" {
		return nil, nil
	}
	p, err := path("plugin-knowledge.require-tags." + slug)
	if err != nil {
		return nil, err
	}
	s, err := readRaw(p)
	if err != nil {
		return nil, err
	}
	return tokens(s), nil
}

// KnowledgeRequireFields: per-slug required OKF metadata fields (e.g. "type").
func KnowledgeRequireFields(slug string) ([]string, error) {
	if slug == "" {
		return nil, nil
	}
	p, err := path("plugin-knowledge.require-fields." + slug)
	if err != nil {
		return nil, err
	}
	s, err := readRaw(p)
	if err != nil {
		return nil, err
	}
	return tokens(s), nil
}

// --- registries ---------------------------------------------------------------

// LocalSoulPresent: the local managed soul is configured (plugin-memory.conf).
func LocalSoulPresent() (bool, error) { return fileExists("plugin-memory.conf") }

// SoulConfigured is an alias used by the knowledge surface for the soul↔system note.
func SoulConfigured() (bool, error) { return fileExists("plugin-memory.conf") }

// ListKnowledgeSystems: registered knowledge-system slugs.
func ListKnowledgeSystems() ([]string, error) {
	p, err := path("knowledge-systems")
	if err != nil {
		return nil, err
	}
	return records(p)
}

// ListRemoteSouls: registered remote-soul slugs (field 1 of each tab-separated row).
func ListRemoteSouls() ([]string, error) {
	p, err := path("souls")
	if err != nil {
		return nil, err
	}
	rows, err := records(p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, strings.SplitN(r, "\t", 2)[0])
	}
	return out, nil
}

// ProjectBinding: the catalog slug bound to DIR, checking DIR then its ancestors
// (nearest wins). "" when unbound.
func ProjectBinding(dir string) (string, error) {
	p, err := path("project-souls")
	if err != nil {
		return "", err
	}
	rows, err := records(p)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		f := strings.SplitN(r, "\t", 2)
		if len(f) == 2 && f[0] != "" {
			m[f[0]] = f[1]
		}
	}
	cur := dir
	for cur != "" {
		if slug, ok := m[cur]; ok {
			return slug, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", nil
}

// --- MCP tool-name parsing ----------------------------------------------------

var markVerbRe = regexp.MustCompile(`mark_([a-z_]+)$`)

// ParsedTool is the server + verb extracted from an MCP tool name.
type ParsedTool struct {
	Server string // "" when un-prefixed (pi prefix "none")
	Verb   string // publish, append, fetch, …
}

// ParseTool handles the harness tool-name shapes:
//
//	mcp__<server>__mark_<verb>   (Claude Code)
//	<server>_mark_<verb>         (pi-mcp-adapter, prefix "server"; hyphens→"_")
//	mark_<verb>                  (pi-mcp-adapter, prefix "none")
func ParseTool(tool string) (ParsedTool, bool) {
	m := markVerbRe.FindStringSubmatch(tool)
	if m == nil {
		return ParsedTool{}, false
	}
	verb := m[1]
	server := strings.TrimSuffix(tool, "mark_"+verb)
	server = strings.TrimPrefix(server, "mcp__")
	server = strings.TrimSuffix(server, "__")
	server = strings.TrimSuffix(server, "_")
	return ParsedTool{Server: server, Verb: verb}, true
}

// norm makes "demarkus-memory" and "demarkus_memory" compare equal.
func norm(s string) string { return strings.ToLower(strings.ReplaceAll(s, "-", "_")) }

func serverIsLocalSoul(server string) bool {
	if server == "" {
		return true // un-prefixed: assume the local soul
	}
	n := norm(server)
	return n == norm(LocalSoulID) || strings.HasSuffix(n, "_"+norm(LocalSoulID))
}

// SoulTargetID: canonical id of the soul a tool writes to (for binding compare),
// or "" when the tool targets a knowledge system or unrelated server.
func SoulTargetID(tool string) (string, error) {
	pt, ok := ParseTool(tool)
	if !ok {
		return "", nil
	}
	if serverIsLocalSoul(pt.Server) {
		return LocalSoulID, nil
	}
	remotes, err := ListRemoteSouls()
	if err != nil {
		return "", err
	}
	for _, slug := range remotes {
		if norm(slug) == norm(pt.Server) {
			return slug, nil
		}
	}
	return "", nil
}

// KnowledgeScope: the registered knowledge-system slug a tool targets, or "".
// The local soul is explicitly NOT a knowledge target.
func KnowledgeScope(tool string) (string, error) {
	pt, ok := ParseTool(tool)
	if !ok {
		return "", nil
	}
	if serverIsLocalSoul(pt.Server) {
		return "", nil
	}
	systems, err := ListKnowledgeSystems()
	if err != nil {
		return "", err
	}
	for _, slug := range systems {
		if norm(slug) == norm(pt.Server) {
			return slug, nil
		}
	}
	return "", nil
}

// TagsHaveAxis: the comma-separated TAGS list contains an "AXIS:value" token.
func TagsHaveAxis(tags, axis string) bool {
	for _, tok := range strings.Split(tags, ",") {
		if strings.HasPrefix(strings.TrimSpace(tok), axis+":") {
			return true
		}
	}
	return false
}
