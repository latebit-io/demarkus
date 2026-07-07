package registry

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// slugSafe is the path-safe alphabet a slug must satisfy (it's embedded in mirror
// file paths and compared against MCP server names).
var slugSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func readRecords(name string) ([]string, error) {
	p, err := config.StatePath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for ln := range strings.SplitSeq(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "#") {
			out = append(out, ln)
		}
	}
	return out, nil
}

// --- soul catalog -------------------------------------------------------------

// SoulRegister upserts a remote-soul row "<slug>\t<host>\t<insecure>\t<tokenFile>",
// keyed on slug. Locked + atomic. Rejects an empty slug/host.
func SoulRegister(slug, host string, insecure bool, tokenFile string) error {
	if slug == "" || host == "" {
		return fmt.Errorf("soul register: slug and host are required")
	}
	if tokenFile == "" {
		tokenFile = "-"
	}
	ins := "0"
	if insecure {
		ins = "1"
	}
	p, err := config.StatePath("souls")
	if err != nil {
		return err
	}
	return withLock(p, func() error {
		rows, err := readRecords("souls")
		if err != nil {
			return err
		}
		var kept []string
		for _, r := range rows {
			if strings.SplitN(r, "\t", 2)[0] != slug {
				kept = append(kept, r)
			}
		}
		kept = append(kept, strings.Join([]string{slug, host, ins, tokenFile}, "\t"))
		return atomicWrite(p, []byte(strings.Join(kept, "\n")+"\n"))
	})
}

// RemoteSoulRow returns the catalog row for SLUG (host, insecure, token file).
// ok is false when the slug isn't registered.
func RemoteSoulRow(slug string) (host string, insecure bool, tokenFile string, ok bool, err error) {
	rows, err := readRecords("souls")
	if err != nil {
		return "", false, "", false, err
	}
	for _, r := range rows {
		f := strings.Split(r, "\t")
		if f[0] == slug {
			if len(f) >= 2 {
				host = f[1]
			}
			if len(f) >= 3 {
				insecure = f[2] == "1"
			}
			if len(f) >= 4 {
				tokenFile = f[3]
			}
			return host, insecure, tokenFile, true, nil
		}
	}
	return "", false, "", false, nil
}

// remoteSoulHost returns the host registered for slug, or "" when not registered.
func remoteSoulHost(slug string) (string, error) {
	rows, err := readRecords("souls")
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		f := strings.Split(r, "\t")
		if f[0] == slug && len(f) >= 2 {
			return f[1], nil
		}
	}
	return "", nil
}

// SoulCatalog emits one row per bindable soul: "<id>\t<tier>\t<host>\t<insecure>".
func SoulCatalog() ([]string, error) {
	var out []string
	local, err := config.LocalSoulPresent()
	if err != nil {
		return nil, err
	}
	if local {
		out = append(out, "demarkus-memory\tlocal\t-\t-")
	}
	rows, err := readRecords("souls")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		f := strings.Split(r, "\t")
		host, ins := "-", "0"
		if len(f) >= 2 {
			host = f[1]
		}
		if len(f) >= 3 {
			ins = f[2]
		}
		out = append(out, fmt.Sprintf("%s\tremote\t%s\t%s", f[0], host, ins))
	}
	return out, nil
}

// IsCatalogSoul reports whether slug is a bindable write target.
func IsCatalogSoul(slug string) (bool, error) {
	if slug == "" {
		return false, nil
	}
	if slug == config.LocalSoulID {
		return config.LocalSoulPresent()
	}
	remotes, err := config.ListRemoteSouls()
	if err != nil {
		return false, err
	}
	if slices.Contains(remotes, slug) {
		return true, nil
	}
	return false, nil
}

// ProjectBindSet records that DIR writes to catalog soul SLUG by default. Locked
// + atomic; validates SLUG is a joined soul first.
func ProjectBindSet(dir, slug string) error {
	ok, err := IsCatalogSoul(slug)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("'%s' is not a joined soul; run /soul-join first (see /soul-default --list)", slug)
	}
	p, err := config.StatePath("project-souls")
	if err != nil {
		return err
	}
	return withLock(p, func() error {
		rows, err := readRecords("project-souls")
		if err != nil {
			return err
		}
		var kept []string
		for _, r := range rows {
			if strings.SplitN(r, "\t", 2)[0] != dir {
				kept = append(kept, r)
			}
		}
		kept = append(kept, dir+"\t"+slug)
		return atomicWrite(p, []byte(strings.Join(kept, "\n")+"\n"))
	})
}

// --- knowledge registry + policy ----------------------------------------------

// KnowledgeRegister appends SLUG to the knowledge-system registry (idempotent).
func KnowledgeRegister(slug string) error {
	if !slugSafe.MatchString(slug) {
		return fmt.Errorf("invalid slug '%s': only [A-Za-z0-9._-] allowed", slug)
	}
	p, err := config.StatePath("knowledge-systems")
	if err != nil {
		return err
	}
	return withLock(p, func() error {
		rows, err := readRecords("knowledge-systems")
		if err != nil {
			return err
		}
		if slices.Contains(rows, slug) {
			return nil // already registered
		}
		rows = append(rows, slug)
		return atomicWrite(p, []byte(strings.Join(rows, "\n")+"\n"))
	})
}

// KnowledgeUnregister is the inverse of KnowledgeRegister: it drops the slug
// from the knowledge-systems registry (so the publish gate stops enforcing on
// it) and clears any mirrored per-slug policy files. Idempotent — removing a
// slug that was never registered is a no-op. `existed` reports whether the slug
// was in the registry.
func KnowledgeUnregister(slug string) (existed bool, err error) {
	if !slugSafe.MatchString(slug) {
		return false, fmt.Errorf("invalid slug '%s': only [A-Za-z0-9._-] allowed", slug)
	}
	p, err := config.StatePath("knowledge-systems")
	if err != nil {
		return false, err
	}
	// Hold the knowledge-systems lock across BOTH the registry mutation and the
	// policy-file cleanup, and have PolicyMirror take the same lock, so an
	// unregister can't race a concurrent re-mirror of the same slug and leave
	// orphaned policy files the gate would keep enforcing.
	err = withLock(p, func() error {
		rows, err := readRecords("knowledge-systems")
		if err != nil {
			return err
		}
		kept := rows[:0]
		for _, r := range rows {
			if r == slug {
				existed = true
				continue
			}
			kept = append(kept, r)
		}
		if !existed {
			return nil
		}
		if len(kept) == 0 {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else if err := atomicWrite(p, []byte(strings.Join(kept, "\n")+"\n")); err != nil {
			return err
		}
		// Clear mirrored policy so a re-join starts clean and the gate doesn't
		// keep enforcing axes/fields for a system that's no longer joined.
		return clearPolicyMirror(slug)
	})
	return existed, err
}

var policyKeys = []string{"strictness", "require_tags", "require_fields"}

func policyFileFor(slug string) map[string]string {
	return map[string]string{
		"strictness":     "plugin-knowledge.strictness." + slug,
		"require_tags":   "plugin-knowledge.require-tags." + slug,
		"require_fields": "plugin-knowledge.require-fields." + slug,
	}
}

// clearPolicyMirror removes every per-slug policy file. Callers must already hold
// the knowledge-systems lock.
func clearPolicyMirror(slug string) error {
	for _, name := range policyFileFor(slug) {
		fp, err := config.StatePath(name)
		if err != nil {
			return err
		}
		if err := os.Remove(fp); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// PolicyMirror parses a knowledge system's policy.md body and mirrors its
// enforced core to the per-slug files the gate reads. A knob absent from the
// policy clears its file (relaxing de-enforces). Idempotent. Serialized against
// KnowledgeUnregister via the shared knowledge-systems lock.
func PolicyMirror(slug, body string) error {
	if !slugSafe.MatchString(slug) {
		return fmt.Errorf("invalid slug '%s': only [A-Za-z0-9._-] allowed", slug)
	}
	lockPath, err := config.StatePath("knowledge-systems")
	if err != nil {
		return err
	}
	return withLock(lockPath, func() error {
		// A queued policy-mirror could run AFTER a knowledge-unregister; under the
		// shared lock, re-confirm the slug is still registered before writing, so
		// we don't resurrect policy files for a system that's no longer joined.
		rows, err := readRecords("knowledge-systems")
		if err != nil {
			return err
		}
		registered := slices.Contains(rows, slug)
		if !registered {
			return clearPolicyMirror(slug)
		}
		fileFor := policyFileFor(slug)
		for _, key := range policyKeys {
			val := policyField(body, key)
			p, err := config.StatePath(fileFor[key])
			if err != nil {
				return err
			}
			if val == "" {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return err
				}
				continue
			}
			if err := atomicWrite(p, []byte(val+"\n")); err != nil {
				return err
			}
		}
		return nil
	})
}

// policyField returns the value of the first body line "KEY: value" where KEY is
// the line's first token (so a prose mention can't match). "" when absent.
func policyField(body, key string) string {
	for ln := range strings.SplitSeq(body, "\n") {
		t := strings.TrimLeft(ln, " \t")
		if strings.HasPrefix(t, key+":") {
			return strings.TrimSpace(t[len(key)+1:])
		}
	}
	return ""
}

// --- promote targets ----------------------------------------------------------

// PromoteTargetAdd registers a plain remote promote target "<slug> <path> [label]".
func PromoteTargetAdd(slug, path, label string) error {
	if slug == "" {
		return fmt.Errorf("add: missing <slug>")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("add: <path> must start with / (got '%s')", path)
	}
	if strings.ContainsAny(path, " \t") {
		return fmt.Errorf("add: <path> must not contain whitespace")
	}
	if !slugSafe.MatchString(slug) {
		return fmt.Errorf("add: <slug> '%s' has unexpected characters", slug)
	}
	p, err := config.StatePath("promote-targets")
	if err != nil {
		return err
	}
	return withLock(p, func() error {
		rows, err := readRecords("promote-targets")
		if err != nil {
			return err
		}
		for _, r := range rows {
			f := strings.Fields(r)
			if len(f) >= 2 && f[0] == slug && f[1] == path {
				return nil // idempotent on slug+path
			}
		}
		line := slug + " " + path
		if label != "" {
			line += " " + label
		}
		rows = append(rows, line)
		return atomicWrite(p, []byte(strings.Join(rows, "\n")+"\n"))
	})
}

// PromoteTargetList returns the registered plain remote promote targets.
func PromoteTargetList() ([]string, error) { return readRecords("promote-targets") }

// DetectPromote lists every promote destination (brokered + plain), or nil when
// none. "knowledge <slug>" / "target <slug> <path> [label]".
func DetectPromote() ([]string, error) {
	var out []string
	systems, err := config.ListKnowledgeSystems()
	if err != nil {
		return nil, err
	}
	for _, s := range systems {
		out = append(out, "knowledge "+s)
	}
	targets, err := PromoteTargetList()
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		out = append(out, "target "+t)
	}
	return out, nil
}

// --- soul-join / knowledge-join orchestration ---------------------------------

var genericLabels = map[string]bool{"mcp": true, "broker": true, "api": true, "gateway": true, "gw": true, "www": true}

// deriveSlug lowercases HOST and returns the first non-generic DNS label, or the
// leftmost label if all are generic; then sanitizes to [a-z0-9-].
func deriveSlug(host string) string {
	host = strings.ToLower(host)
	labels := strings.Split(host, ".")
	raw := ""
	for _, l := range labels {
		if !genericLabels[l] {
			raw = l
			break
		}
	}
	if raw == "" && len(labels) > 0 {
		raw = labels[0]
	}
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(collapseDash(b.String()), "-")
}

func collapseDash(s string) string {
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return s
}

// KnowledgeJoinResult is the validated broker info for /knowledge-join.
type KnowledgeJoinResult struct {
	URL    string
	Slug   string
	McpURL string
}

// KnowledgeJoin validates a broker URL (https + RFC 9728 metadata) and derives a
// slug. Returns an error message suitable to show the user verbatim on failure.
func KnowledgeJoin(rawURL string) (*KnowledgeJoinResult, error) {
	u := strings.TrimSpace(rawURL)
	low := strings.ToLower(u)
	allowHTTP := os.Getenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP") == "1"
	switch {
	case strings.HasPrefix(low, "https://"):
	case strings.HasPrefix(low, "http://"):
		if !allowHTTP {
			return nil, fmt.Errorf("broker URL must use https:// (got: %s)", u)
		}
	default:
		return nil, fmt.Errorf("broker URL must start with https:// (got: %s)", u)
	}
	u = strings.TrimRight(u, "/")
	// normalize scheme to lowercase
	if i := strings.Index(u, "://"); i > 0 {
		u = strings.ToLower(u[:i]) + u[i:]
	}

	meta := u + "/.well-known/oauth-protected-resource"
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Head(meta)
	if err != nil {
		return nil, fmt.Errorf("broker unreachable at %s (DNS / TCP / TLS failure; check the URL and network)", meta)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 401, 403:
		return nil, fmt.Errorf("broker rejected metadata fetch with HTTP %d (the metadata endpoint should be public; confirm this is a Slice 7+ demarkus-broker)", resp.StatusCode)
	case 404:
		return nil, fmt.Errorf("broker has no /.well-known/oauth-protected-resource (HTTP 404); not a Slice 7+ demarkus-broker, or the URL points at the management API instead of the MCP host")
	default:
		return nil, fmt.Errorf("broker metadata fetch returned HTTP %d (expected 200)", resp.StatusCode)
	}

	host := u[strings.Index(u, "://")+3:]
	host = strings.SplitN(host, "/", 2)[0]
	host = strings.SplitN(host, ":", 2)[0]
	slug := deriveSlug(host)
	if slug == "" {
		return nil, fmt.Errorf("could not derive a usable MCP server slug from %s (host: %s)", u, host)
	}
	return &KnowledgeJoinResult{URL: u, Slug: slug, McpURL: u + "/mcp"}, nil
}

// SoulJoinResult is the outcome of a successful /soul-join.
type SoulJoinResult struct {
	Slug      string
	Host      string
	Insecure  bool
	TokenFile string
}

// SoulJoin normalizes a remote-soul host, derives a slug, writes the token to a
// 0600 file, registers the catalog row (rejecting a slug collision to a different
// host), and optionally binds the project. The token never transits any channel
// but this call's argument + the 0600 file.
func SoulJoin(rawHost, token string, insecure bool, bindDir string) (*SoulJoinResult, error) {
	h := strings.TrimSpace(rawHost)
	low := strings.ToLower(h)
	if strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://") {
		return nil, fmt.Errorf("'%s' is an HTTPS URL; use /knowledge-join for broker-fronted knowledge systems", h)
	}
	h = strings.TrimPrefix(h, "mark://")
	h = strings.TrimRight(h, "/")
	hostOnly := strings.SplitN(strings.SplitN(h, "/", 2)[0], ":", 2)[0]
	slug := deriveSlug(hostOnly)
	if slug == "" {
		return nil, fmt.Errorf("could not derive a slug from host '%s'", rawHost)
	}
	if slug == config.LocalSoulID {
		return nil, fmt.Errorf("slug '%s' is reserved for the local managed soul; join a host with a different first label", config.LocalSoulID)
	}
	host := "mark://" + h

	existingHost, err := remoteSoulHost(slug)
	if err != nil {
		return nil, err
	}
	if existingHost != "" && existingHost != host {
		return nil, fmt.Errorf("soul slug '%s' is already joined for host '%s'; joining '%s' would retarget every project bound to it; remove the existing entry or join from a host with a different first label", slug, existingHost, host)
	}

	tokenFile := "-"
	if token != "" {
		tf, err := config.StatePath("soul-" + slug + ".token")
		if err != nil {
			return nil, err
		}
		if err := atomicWrite0600(tf, []byte(token)); err != nil {
			return nil, err
		}
		tokenFile = tf
	}
	if err := SoulRegister(slug, host, insecure, tokenFile); err != nil {
		return nil, err
	}
	if bindDir != "" {
		if err := ProjectBindSet(bindDir, slug); err != nil {
			return nil, err
		}
	}
	return &SoulJoinResult{Slug: slug, Host: host, Insecure: insecure, TokenFile: tokenFile}, nil
}

func atomicWrite0600(path string, data []byte) error {
	return atomicWritePerm(path, data, 0o600)
}
