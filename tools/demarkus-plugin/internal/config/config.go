// Package config reads the shared ~/.demarkus plugin state — the single source
// of truth that every demarkus plugin (Claude Code, pi, Codex, …) consults via
// the demarkus-plugin binary. Readers fail open ONLY on "file absent" (ENOENT);
// any other I/O error propagates so a present-but-unreadable config can't
// silently disable a gate.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Strictness is the enforcement level a gate applies to a violation.
type Strictness string

// Enforcement levels a gate can apply to a violation.
const (
	Warn  Strictness = "warn"
	Block Strictness = "block"
	Ask   Strictness = "ask"
)

// LocalSoulID is the reserved catalog id / MCP server name of the local managed soul.
const LocalSoulID = "demarkus-memory"

// Home returns the ~/.demarkus base directory that all plugin paths resolve under.
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
	for ln := range strings.SplitSeq(s, "\n") {
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

// MemoryTagStrictness env override → plugin-memory.strictness → warn (floor).
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

// MemoryDestStrictness env override → plugin-memory.dest-strictness → block.
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

// StyleStrictness is the enforcement level for the documentation-style guard
// (frontmatter fence in the body, missing H1, em dashes, duplicate headings).
// env override → plugin.style-strictness → warn: style steers, it does not
// wall; the reasons teach the rule at the moment it is broken.
func StyleStrictness() (Strictness, error) {
	if v := os.Getenv("DEMARKUS_STYLE_STRICTNESS"); v != "" {
		return validStrictness(v, Warn), nil
	}
	p, err := path("plugin.style-strictness")
	if err != nil {
		return Warn, err
	}
	s, err := readTrimmed(p)
	if err != nil {
		return Warn, err
	}
	return validStrictness(s, Warn), nil
}

// RetentionStrictness is the enforcement level for the retention guard (a
// publish/append whose metadata carries a prunable retention value permanently
// deletes older versions). env override → plugin.retention-strictness → ask:
// destructive-by-metadata warrants a human decision by default, on every
// surface the gate covers (soul and knowledge alike).
func RetentionStrictness() (Strictness, error) {
	if v := os.Getenv("DEMARKUS_RETENTION_STRICTNESS"); v != "" {
		return validStrictness(v, Ask), nil
	}
	p, err := path("plugin.retention-strictness")
	if err != nil {
		return Ask, err
	}
	s, err := readTrimmed(p)
	if err != nil {
		return Ask, err
	}
	return validStrictness(s, Ask), nil
}

// UpdateCheckEnabled reports whether the plugin update check may run.
// env override → plugin.update-check → on: a notice once a day is cheap, and a
// user who wants it gone gets a file, not just an env var a GUI launch drops.
func UpdateCheckEnabled() (bool, error) {
	if v := os.Getenv("DEMARKUS_UPDATE_CHECK"); v != "" {
		return !isOff(v), nil
	}
	p, err := path("plugin.update-check")
	if err != nil {
		return true, err
	}
	s, err := readTrimmed(p)
	if err != nil {
		return true, err
	}
	return !isOff(s), nil
}

func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

// KnowledgeStrictness env override → plugin-knowledge.strictness.<slug> → warn.
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

// KnowledgeRequireTags per-slug required tag axes (e.g. "category team").
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

// KnowledgeRequireFields per-slug required OKF metadata fields (e.g. "type").
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

// LocalSoulPresent the local managed soul is configured (plugin-memory.conf).
func LocalSoulPresent() (bool, error) { return fileExists("plugin-memory.conf") }

// SoulConfigured is an alias used by the knowledge surface for the soul↔system note.
func SoulConfigured() (bool, error) { return fileExists("plugin-memory.conf") }

// ListKnowledgeSystems registered knowledge-system slugs.
func ListKnowledgeSystems() ([]string, error) {
	p, err := path("knowledge-systems")
	if err != nil {
		return nil, err
	}
	return records(p)
}

// ListRemoteSouls registered remote-soul slugs (field 1 of each tab-separated row).
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

// ProjectBinding the catalog slug bound to DIR, checking DIR then its ancestors
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

// SoulTargetID canonical id of the soul a tool writes to (for binding compare),
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

// KnowledgeScope the registered knowledge-system slug a tool targets, or "".
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

// TagsHaveAxis the comma-separated TAGS list contains an "AXIS:value" token.
func TagsHaveAxis(tags, axis string) bool {
	for tok := range strings.SplitSeq(tags, ",") {
		if strings.HasPrefix(strings.TrimSpace(tok), axis+":") {
			return true
		}
	}
	return false
}

// --- promote destinations + knowledge presence (for nudges) -------------------

// KnowledgePresent at least one knowledge system is joined.
func KnowledgePresent() (bool, error) {
	systems, err := ListKnowledgeSystems()
	return len(systems) > 0, err
}

// PromoteTargets registered plain remote promote targets (one per line).
func PromoteTargets() ([]string, error) {
	p, err := path("promote-targets")
	if err != nil {
		return nil, err
	}
	return records(p)
}

// PromoteDestinationPresent any promote destination exists — a brokered
// knowledge system OR a plain remote target. With none, promote is dormant.
func PromoteDestinationPresent() (bool, error) {
	kp, err := KnowledgePresent()
	if err != nil {
		return false, err
	}
	if kp {
		return true, nil
	}
	targets, err := PromoteTargets()
	return len(targets) > 0, err
}

// PublishGateScopeLocal true when a tool targets a soul this plugin routes
// (local managed soul or a registered remote soul) — i.e. SoulTargetID != "".
func PublishGateScopeLocal(tool string) (bool, error) {
	id, err := SoulTargetID(tool)
	return id != "", err
}

// --- managed-server config (plugin-memory.conf) -------------------------------

// PluginConfig holds the SOUL_DIR/PORT/MODE the local managed server runs under.
type PluginConfig struct {
	SoulDir string
	Port    string
	Mode    string
}

// LoadConfig parses the shell-style plugin-memory.conf (KEY=value lines, possibly
// printf %q-quoted). Returns nil when the file is absent or incomplete.
func LoadConfig() (*PluginConfig, error) {
	p, err := path("plugin-memory.conf")
	if err != nil {
		return nil, err
	}
	raw, err := readRaw(p)
	if err != nil || raw == "" {
		return nil, err
	}
	out := map[string]string{}
	for ln := range strings.SplitSeq(raw, "\n") {
		for _, key := range []string{"SOUL_DIR", "PORT", "MODE"} {
			if after, ok := strings.CutPrefix(ln, key+"="); ok {
				v := strings.TrimSpace(after)
				v = unquoteShell(v)
				out[key] = v
			}
		}
	}
	if out["SOUL_DIR"] == "" || out["PORT"] == "" || out["MODE"] == "" {
		return nil, nil
	}
	return &PluginConfig{SoulDir: out["SOUL_DIR"], Port: out["PORT"], Mode: out["MODE"]}, nil
}

// unquoteShell undoes a printf %q quoting for the simple values we store
// (paths/ints/words): a $'…' or '…' wrapper, then backslash-unescape.
func unquoteShell(v string) string {
	if strings.HasPrefix(v, "$'") && strings.HasSuffix(v, "'") && len(v) >= 3 {
		v = v[2 : len(v)-1]
	} else if strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") && len(v) >= 2 {
		v = v[1 : len(v)-1]
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// StatePath returns the absolute path of a file under ~/.demarkus (e.g. a
// sentinel). Exported so the guidance command can manage one-time markers.
func StatePath(name string) (string, error) { return path(name) }

// NormalizeCall unwraps the pi-mcp-adapter "mcp" proxy: when the tool is the
// literal "mcp", the real tool name is input.tool and the real args are
// input.args (a JSON string or an object). Direct calls pass through unchanged.
// Shared by the gate and nudge commands so the unwrap lives in one place.
func NormalizeCall(tool string, input map[string]any) (name string, args map[string]any) {
	if input == nil {
		input = map[string]any{}
	}
	if tool != "mcp" {
		return tool, input
	}
	t, ok := input["tool"].(string)
	if !ok || t == "" {
		return tool, input
	}
	switch raw := input["args"].(type) {
	case string:
		var parsed map[string]any
		if json.Unmarshal([]byte(raw), &parsed) == nil {
			return t, parsed
		}
		return t, map[string]any{}
	case map[string]any:
		return t, raw
	default:
		return t, map[string]any{}
	}
}
