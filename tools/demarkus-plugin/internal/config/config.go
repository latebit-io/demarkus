// Package config reads shared plugin state from ~/.demarkus.
// Only absent files are defaults; every other I/O error propagates.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalMemoryID is the reserved catalog id / MCP server name of the local managed memory.
const LocalMemoryID = "demarkus-memory"

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

// Records returns the non-blank, non-comment, trimmed lines of a state file.
func Records(name string) ([]string, error) {
	p, err := path(name)
	if err != nil {
		return nil, err
	}
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

func stateExists(name string) (bool, error) {
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

// Knob is a setting's value: the env override, else the trimmed content of
// the state file ("" when absent). An unreadable file is the error.
func Knob(env, file string) (string, error) {
	if v := os.Getenv(env); v != "" {
		return v, nil
	}
	p, err := path(file)
	if err != nil {
		return "", err
	}
	return readTrimmed(p)
}

// Brand is a branded plugin's context/brand.json, as the generator writes it
// (tools/plugin-prompts brandNames): the status line's words, gate defaults.
type Brand struct {
	PluginName      string `json:"plugin_name"`
	Store           string `json:"store_noun"`
	TaintStrictness string `json:"taint_strictness"`
}

// BrandFileEnv names the brand.json a plugin's gate hooks point at.
const BrandFileEnv = "DEMARKUS_BRAND_FILE"

// ReadBrand overlays the brand file at p onto b. A key it lacks keeps b's
// value, and so does a missing file: a base plugin ships none.
func ReadBrand(p string, b *Brand) error {
	// Not readRaw: gosec follows the hook's env path into it (G703), and the
	// waiver belongs here, where whoever sets the path could set the knob.
	raw, err := os.ReadFile(p) //nolint:gosec // see above
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read brand.json: %w", err)
	}
	if err := json.Unmarshal(raw, b); err != nil {
		return fmt.Errorf("parse brand.json %s: %w", p, err)
	}
	return nil
}

// HookBrand is the brand of the plugin whose gate hook is running; zero for
// a base plugin.
func HookBrand() (b Brand, err error) {
	if p := os.Getenv(BrandFileEnv); p != "" {
		err = ReadBrand(p, &b)
	}
	return b, err
}

// IsOff reports a knob value that turns its feature off.
func IsOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

// --- registries ---------------------------------------------------------------

// LocalMemoryPresent the local managed memory is configured (plugin-memory.conf).
func LocalMemoryPresent() (bool, error) { return stateExists("plugin-memory.conf") }

// MemoryConfigured is an alias used by the knowledge surface for the memory↔system note.
func MemoryConfigured() (bool, error) { return stateExists("plugin-memory.conf") }

// ListKnowledgeSystems registered knowledge-system slugs.
func ListKnowledgeSystems() ([]string, error) { return Records("knowledge-systems") }

// MemoryRow is one record of the remote memory catalog (the `souls` file):
// "<slug>\t<host>\t<insecure>\t<tokenFile>".
type MemoryRow struct {
	Slug      string
	Host      string
	Insecure  bool
	TokenFile string // "-" (or "") when the row has none
}

// ParseMemoryRow decodes a catalog record; fields after the slug may be absent.
func ParseMemoryRow(record string) MemoryRow {
	f := strings.Split(record, "\t")
	row := MemoryRow{Slug: f[0]}
	if len(f) >= 2 {
		row.Host = f[1]
	}
	if len(f) >= 3 {
		row.Insecure = f[2] == "1"
	}
	if len(f) >= 4 {
		row.TokenFile = f[3]
	}
	return row
}

// Record encodes the row as the catalog stores it.
func (r MemoryRow) Record() string {
	ins := "0"
	if r.Insecure {
		ins = "1"
	}
	return strings.Join([]string{r.Slug, r.Host, ins, r.TokenFile}, "\t")
}

// IsBroker reports whether the row names an HTTPS memory broker.
func (r MemoryRow) IsBroker() bool { return IsBrokerHost(r.Host) }

// IsBrokerHost reports whether a memory host is an HTTPS memory-broker
// endpoint (OAuth in the MCP client; never served by mcp-serve).
func IsBrokerHost(host string) bool {
	low := strings.ToLower(host)
	return strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://")
}

// RemoteMemoryRows is the catalog, one row per registered remote memory.
func RemoteMemoryRows() ([]MemoryRow, error) {
	records, err := Records("souls")
	if err != nil {
		return nil, err
	}
	rows := make([]MemoryRow, 0, len(records))
	for _, r := range records {
		rows = append(rows, ParseMemoryRow(r))
	}
	return rows, nil
}

// ListRemoteMemories registered remote-memory slugs.
func ListRemoteMemories() ([]string, error) {
	rows, err := RemoteMemoryRows()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Slug)
	}
	return out, nil
}

// ProjectBinding the catalog slug bound to DIR, checking DIR then its ancestors
// (nearest wins). "" when unbound.
func ProjectBinding(dir string) (string, error) {
	rows, err := Records("project-souls")
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

// --- MCP server names ---------------------------------------------------------

// norm makes "demarkus-memory" and "demarkus_memory" compare equal.
func norm(s string) string { return strings.ToLower(strings.ReplaceAll(s, "-", "_")) }

// SameName reports two server or slug names equal up to case and hyphens.
func SameName(a, b string) bool { return norm(a) == norm(b) }

// LocalMemoryAliasFile holds "<write-token> <alias>": the MCP server name a
// branded plugin serves the local memory under (empty for the default) and the
// write's token, so a failed start restores only its own record.
const LocalMemoryAliasFile = "local-memory-alias"

// LocalMemoryAliasRecord the stored alias ("" for the default name) and write token.
func LocalMemoryAliasRecord() (alias, token string, err error) {
	p, err := path(LocalMemoryAliasFile)
	if err != nil {
		return "", "", err
	}
	s, err := readRaw(p)
	if err != nil {
		return "", "", err
	}
	// Only line ends trimmed: an empty alias is "<token> " and must keep its space.
	token, alias, found := strings.Cut(strings.TrimRight(s, "\r\n"), " ")
	if !found {
		return strings.TrimSpace(token), "", nil // token-less record: alias only
	}
	return alias, token, nil
}

// LocalMemoryAlias the recorded alias, or "" when the default name is in use.
func LocalMemoryAlias() (string, error) {
	alias, _, err := LocalMemoryAliasRecord()
	return alias, err
}

// ServerMatches reports whether an MCP server name resolves to id: the name
// itself, or "<plugin>_<id>" as Claude Code prefixes plugin servers. Hyphens
// and underscores compare equal.
func ServerMatches(server, id string) bool {
	n := norm(server)
	return n == norm(id) || strings.HasSuffix(n, "_"+norm(id))
}

// ResolvesToLocalMemory reports whether an MCP server name routes to the local
// memory: the reserved id or the recorded alias, exact or plugin-prefixed.
// Catalog inserts reject slugs by the same predicate, so none can route as local.
func ResolvesToLocalMemory(server string) (bool, error) {
	if ServerMatches(server, LocalMemoryID) {
		return true, nil
	}
	alias, err := LocalMemoryAlias()
	if err != nil || alias == "" {
		return false, err
	}
	return ServerMatches(server, alias), nil
}

// --- promote destinations + knowledge presence (for nudges) -------------------

// KnowledgePresent at least one knowledge system is joined.
func KnowledgePresent() (bool, error) {
	systems, err := ListKnowledgeSystems()
	return len(systems) > 0, err
}

// PromoteTargets registered plain remote promote targets (one per line).
func PromoteTargets() ([]string, error) { return Records("promote-targets") }

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

// --- managed-server config (plugin-memory.conf) -------------------------------

// PluginConfig holds the SOUL_DIR/PORT/MODE the local managed server runs under.
// TokensTOML records the token registry resolved at setup (reuse mode may adopt
// a server whose -tokens points outside the root); empty on older configs.
type PluginConfig struct {
	MemoryDir  string
	Port       string
	Mode       string
	TokensTOML string
}

// ErrConfigIncomplete marks a plugin-memory.conf that exists but lacks a required key.
var ErrConfigIncomplete = errors.New("plugin-memory.conf is incomplete")

// LoadConfig parses the shell-style plugin-memory.conf (KEY=value lines, possibly
// printf %q-quoted). Nil means absent or empty; a damaged file is an error.
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
		for _, key := range []string{"SOUL_DIR", "PORT", "MODE", "TOKENS"} {
			if after, ok := strings.CutPrefix(ln, key+"="); ok {
				v := strings.TrimSpace(after)
				v = unquoteShell(v)
				out[key] = v
			}
		}
	}
	for _, key := range []string{"SOUL_DIR", "PORT", "MODE"} {
		if out[key] == "" {
			return nil, fmt.Errorf("%w: %s has no %s; fix or remove it, then run /soul-init", ErrConfigIncomplete, p, key)
		}
	}
	return &PluginConfig{MemoryDir: out["SOUL_DIR"], Port: out["PORT"], Mode: out["MODE"], TokensTOML: out["TOKENS"]}, nil
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
