package registry

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	pathpkg "path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/latebit-io/demarkus/client/joinurl"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
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

// RemoteSoulRow returns the catalog row for SLUG.
// ok is false when the slug isn't registered.
func RemoteSoulRow(slug string) (SoulRow, bool, error) {
	rows, err := readRecords("souls")
	if err != nil {
		return SoulRow{}, false, err
	}
	for _, r := range rows {
		f := strings.Split(r, "\t")
		if f[0] == slug {
			if len(f) < 2 || f[1] == "" {
				return SoulRow{}, false, fmt.Errorf("souls catalog row for %q has no host; re-run /soul-join", slug)
			}
			var row SoulRow
			if len(f) >= 2 {
				row.Host = f[1]
			}
			if len(f) >= 3 {
				row.Insecure = f[2] == "1"
			}
			if len(f) >= 4 {
				row.TokenFile = f[3]
			}
			return row, true, nil
		}
	}
	return SoulRow{}, false, nil
}

// SoulRow is one souls-catalog record.
type SoulRow struct {
	Host      string
	Insecure  bool
	TokenFile string
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
	soulsPath, err := config.StatePath("souls")
	if err != nil {
		return err
	}
	bindingsPath, err := config.StatePath("project-souls")
	if err != nil {
		return err
	}
	return withLock(soulsPath, func() error {
		return withLock(bindingsPath, func() error {
			ok, err := IsCatalogSoul(slug)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("'%s' is not a joined soul; run /soul-join first (see /soul-default --list)", slug)
			}
			mutation, err := prepareProjectBindingMutation(dir, slug, bindingsPath)
			if err != nil {
				return err
			}
			return applyStateMutations([]stateMutation{mutation})
		})
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
		"policy":         "plugin-knowledge.policy." + slug,
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

// PolicyMirror mirrors a knowledge system's enforceable policy to per-slug files.
// Missing directives clear their files. The registry lock serializes unregisters.
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
		policy := publishpolicy.Parse(body)
		if err := policy.Validate(); err != nil {
			return err
		}
		values := map[string]string{
			"strictness":     string(policy.Strictness),
			"require_tags":   strings.Join(policy.RequiredTagAxes, " "),
			"require_fields": strings.Join(policy.RequiredFields, " "),
		}
		fileFor := policyFileFor(slug)
		snapshotPath, err := config.StatePath(fileFor["policy"])
		if err != nil {
			return err
		}
		snapshot, err := prepareStateMutation(snapshotPath, policySnapshot(policy, values), 0o644)
		if err != nil {
			return err
		}
		var mutations []stateMutation
		for _, key := range policyKeys {
			val := values[key]
			p, err := config.StatePath(fileFor[key])
			if err != nil {
				return err
			}
			var mutation stateMutation
			if val == "" {
				mutation, err = prepareDeleteStateMutation(p)
				if err != nil {
					return err
				}
			} else {
				mutation, err = prepareStateMutation(p, []byte(val+"\n"), 0o644)
				if err != nil {
					return err
				}
			}
			mutations = append(mutations, mutation)
		}
		mutations = append(mutations, snapshot)
		return applyStateMutations(mutations)
	})
}

func policySnapshot(policy publishpolicy.Policy, values map[string]string) []byte {
	var body strings.Builder
	body.WriteString("# Atomic mirrored publish policy.\n")
	if policy.Strictness != "" {
		fmt.Fprintf(&body, "strictness: %s\n", values["strictness"])
	}
	if len(policy.RequiredTagAxes) > 0 {
		fmt.Fprintf(&body, "require_tags: %s\n", values["require_tags"])
	}
	if len(policy.RequiredFields) > 0 {
		fmt.Fprintf(&body, "require_fields: %s\n", values["require_fields"])
	}
	return []byte(body.String())
}

// --- promote targets ----------------------------------------------------------

// PromoteTargetAdd registers a plain remote promote target "<slug> <path> [label]".
// It returns the canonical stored path.
func PromoteTargetAdd(slug, path, label string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("add: missing <slug>")
	}
	canonicalPath, err := canonicalPromotePath(path)
	if err != nil {
		return "", err
	}
	path = canonicalPath
	if !slugSafe.MatchString(slug) {
		return "", fmt.Errorf("add: <slug> '%s' has unexpected characters", slug)
	}
	if err := validateRecordField("promote label", label); err != nil {
		return "", err
	}
	p, err := config.StatePath("promote-targets")
	if err != nil {
		return "", err
	}
	err = withLock(p, func() error {
		rows, err := readRecords("promote-targets")
		if err != nil {
			return err
		}
		for i, row := range rows {
			fields := strings.SplitN(row, " ", 3)
			if len(fields) < 2 || fields[0] != slug {
				continue
			}
			existingPath, pathErr := canonicalPromotePath(fields[1])
			if pathErr != nil || existingPath != path {
				continue
			}
			if fields[1] == path {
				return nil // idempotent on slug+canonical path
			}
			rows[i] = slug + " " + path
			if len(fields) == 3 {
				rows[i] += " " + fields[2]
			}
			return atomicWrite(p, []byte(strings.Join(rows, "\n")+"\n"))
		}
		line := slug + " " + path
		if label != "" {
			line += " " + label
		}
		rows = append(rows, line)
		return atomicWrite(p, []byte(strings.Join(rows, "\n")+"\n"))
	})
	return path, err
}

func canonicalPromotePath(raw string) (string, error) {
	if !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("add: <path> must start with / (got '%s')", raw)
	}
	if strings.ContainsAny(raw, "\\\x00") || strings.ContainsFunc(raw, unicode.IsSpace) {
		return "", fmt.Errorf("add: <path> must not contain whitespace, backslashes, or NUL")
	}
	for segment := range strings.SplitSeq(raw, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("add: <path> must not contain traversal segments")
		}
	}
	return pathpkg.Clean(raw), nil
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
		return nil, fmt.Errorf("broker rejected metadata fetch with HTTP %d (the metadata endpoint should be public; confirm this is a Slice 7+ demarkus-knowledge-broker)", resp.StatusCode)
	case 404:
		return nil, fmt.Errorf("broker has no /.well-known/oauth-protected-resource (HTTP 404); not a Slice 7+ demarkus-knowledge-broker, or the URL points at the management API instead of the MCP host")
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
	// Broker marks an HTTPS memory-broker soul (OAuth at the endpoint,
	// registered with the harness's HTTP MCP transport, never mcp-serve);
	// McpURL is then the endpoint to register.
	Broker bool
	McpURL string
}

// SoulJoin normalizes a remote-soul host, derives a slug, writes the token to a
// 0600 file, registers the catalog row (rejecting a slug collision to a different
// host), and optionally binds the project. The token never transits any channel
// but this call's argument + the 0600 file.
//
// rawHost may also be a join URL (mark://host#token=...) as emitted by
// install.sh or demarkus-token join; its fragment supplies the token.
func SoulJoin(rawHost, token string, insecure bool, bindDir string) (*SoulJoinResult, error) {
	h := strings.TrimSpace(rawHost)
	low := strings.ToLower(h)
	if strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://") {
		return soulJoinBroker(h, token, bindDir)
	}
	// Every host form goes through joinurl.Parse so malformed input (paths,
	// userinfo, query, IPv6 literals) is rejected instead of half-parsed
	// into a broken catalog row.
	j, err := joinurl.Parse(h)
	if err != nil {
		return nil, err
	}
	if j.Token != "" {
		if token != "" && token != j.Token {
			return nil, fmt.Errorf("both explicit token input and a join-URL token were given; pass one")
		}
		token = j.Token
	}
	h = j.Host
	hostOnly := strings.SplitN(h, ":", 2)[0]
	slug := deriveSlug(hostOnly)
	if slug == "" {
		return nil, fmt.Errorf("could not derive a slug from host '%s'", rawHost)
	}
	if slug == config.LocalSoulID {
		return nil, fmt.Errorf("slug '%s' is reserved for the local managed soul; join a host with a different first label", config.LocalSoulID)
	}
	host := "mark://" + h
	soulsPath, err := config.StatePath("souls")
	if err != nil {
		return nil, err
	}
	bindingsPath := ""
	if bindDir != "" {
		bindingsPath, err = config.StatePath("project-souls")
		if err != nil {
			return nil, err
		}
	}

	managedTokenFile, err := config.StatePath("soul-" + slug + ".token")
	if err != nil {
		return nil, err
	}
	tokenFile := "-"
	if token != "" {
		tokenFile = managedTokenFile
	}
	if err := commitSoulJoin(slug, host, token, tokenFile, managedTokenFile, bindDir, soulsPath, bindingsPath, insecure); err != nil {
		return nil, err
	}
	return &SoulJoinResult{Slug: slug, Host: host, Insecure: insecure, TokenFile: tokenFile}, nil
}

// soulJoinBroker joins an HTTPS memory-broker soul: /knowledge-join's
// metadata validation, but the row lands in the SOULS catalog (gate +
// binding apply), no token file; OAuth happens in the MCP client.
func soulJoinBroker(rawURL, token, bindDir string) (*SoulJoinResult, error) {
	if token != "" {
		return nil, fmt.Errorf("a memory-broker soul authenticates via OAuth in the MCP client; do not pass a token")
	}
	validated, err := KnowledgeJoin(rawURL)
	if err != nil {
		return nil, err
	}
	slug := validated.Slug
	if slug == config.LocalSoulID {
		return nil, fmt.Errorf("slug '%s' is reserved for the local managed soul; front the broker at a host with a different first label", config.LocalSoulID)
	}
	soulsPath, err := config.StatePath("souls")
	if err != nil {
		return nil, err
	}
	bindingsPath := ""
	if bindDir != "" {
		bindingsPath, err = config.StatePath("project-souls")
		if err != nil {
			return nil, err
		}
	}
	managedTokenFile, err := config.StatePath("soul-" + slug + ".token")
	if err != nil {
		return nil, err
	}
	if err := commitSoulJoin(slug, validated.URL, "", "-", managedTokenFile, bindDir, soulsPath, bindingsPath, false); err != nil {
		return nil, err
	}
	return &SoulJoinResult{Slug: slug, Host: validated.URL, TokenFile: "-", Broker: true, McpURL: validated.McpURL}, nil
}

// Every multi-file registry path locks souls before project-souls; never reverse.
func commitSoulJoin(slug, host, token, tokenFile, managedTokenFile, bindDir, soulsPath, bindingsPath string, insecure bool) error {
	mutate := func() error {
		mutations, replacedExternal, err := prepareSoulJoinMutations(slug, host, token, tokenFile, managedTokenFile, bindDir, soulsPath, bindingsPath, insecure)
		if err != nil {
			return err
		}
		if err := applyStateMutations(mutations); err != nil {
			return err
		}
		if replacedExternal != "" {
			if _, err := fmt.Fprintf(os.Stderr, "soul %s: catalog reference moved from %s to %s; the old external token file still exists\n", slug, replacedExternal, managedTokenFile); err != nil {
				return fmt.Errorf("soul join committed but reporting replaced external token reference failed: %w", err)
			}
		}
		return nil
	}
	return withLock(soulsPath, func() error {
		if bindingsPath == "" {
			return mutate()
		}
		return withLock(bindingsPath, mutate)
	})
}

func prepareSoulJoinMutations(slug, host, token, tokenFile, managedTokenFile, bindDir, soulsPath, bindingsPath string, insecure bool) ([]stateMutation, string, error) {
	existing, exists, err := RemoteSoulRow(slug)
	if err != nil {
		return nil, "", err
	}
	if exists && existing.Host != host {
		return nil, "", fmt.Errorf("soul slug '%s' is already joined for host '%s'; joining '%s' would retarget every project bound to it; remove the existing entry or join from a host with a different first label", slug, existing.Host, host)
	}
	replacedExternal, err := replacedExternalToken(slug, token, managedTokenFile, existing, exists)
	if err != nil {
		return nil, "", err
	}
	rows, err := readRecords("souls")
	if err != nil {
		return nil, "", err
	}
	kept := make([]string, 0, len(rows)+1)
	for _, row := range rows {
		if strings.SplitN(row, "\t", 2)[0] != slug {
			kept = append(kept, row)
		}
	}
	ins := "0"
	if insecure {
		ins = "1"
	}
	kept = append(kept, strings.Join([]string{slug, host, ins, tokenFile}, "\t"))

	mutations := make([]stateMutation, 0, 3)
	var tokenDelete *stateMutation
	if token != "" {
		mutation, err := prepareStateMutation(tokenFile, []byte(token), 0o600)
		if err != nil {
			return nil, "", err
		}
		mutations = append(mutations, mutation)
	} else {
		mutation, err := prepareDeleteStateMutation(managedTokenFile)
		if err != nil {
			return nil, "", err
		}
		tokenDelete = &mutation
	}
	catalog, err := prepareStateMutation(soulsPath, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
	if err != nil {
		return nil, "", err
	}
	mutations = append(mutations, catalog)
	// Publish the tokenless catalog row before removing the old token so readers
	// never observe a token-backed row whose credential file is already gone.
	if tokenDelete != nil {
		mutations = append(mutations, *tokenDelete)
	}
	if bindDir != "" {
		binding, err := prepareProjectBindingMutation(bindDir, slug, bindingsPath)
		if err != nil {
			return nil, "", err
		}
		mutations = append(mutations, binding)
	}
	return mutations, replacedExternal, nil
}

func replacedExternalToken(slug, token, managedTokenFile string, existing SoulRow, exists bool) (string, error) {
	if !exists || existing.TokenFile == "" || existing.TokenFile == "-" || existing.TokenFile == managedTokenFile {
		return "", nil
	}
	if token == "" {
		return "", fmt.Errorf("soul '%s' uses externally managed token file %q; refusing to remove its catalog reference during tokenless rejoin; resolve that credential explicitly first", slug, existing.TokenFile)
	}
	return existing.TokenFile, nil
}

func prepareProjectBindingMutation(dir, slug, path string) (stateMutation, error) {
	if err := validateRecordField("project binding directory", dir); err != nil {
		return stateMutation{}, err
	}
	bindings, err := readRecords("project-souls")
	if err != nil {
		return stateMutation{}, err
	}
	bound := make([]string, 0, len(bindings)+1)
	for _, row := range bindings {
		if strings.SplitN(row, "\t", 2)[0] != dir {
			bound = append(bound, row)
		}
	}
	bound = append(bound, dir+"\t"+slug)
	return prepareStateMutation(path, []byte(strings.Join(bound, "\n")+"\n"), 0o644)
}

func validateRecordField(name, value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not start or end with whitespace", name)
	}
	if strings.ContainsAny(value, "\t\r\n\x00") {
		return fmt.Errorf("%s must not contain tab, line break, or NUL delimiters", name)
	}
	return nil
}

type fileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

type stateMutation struct {
	before fileSnapshot
	data   []byte
	mode   os.FileMode
	delete bool
}

var writeStateFile = atomicWritePerm

func prepareStateMutation(path string, data []byte, mode os.FileMode) (stateMutation, error) {
	snapshot, err := snapshotFile(path)
	if err != nil {
		return stateMutation{}, err
	}
	return stateMutation{before: snapshot, data: data, mode: mode}, nil
}

func prepareDeleteStateMutation(path string) (stateMutation, error) {
	snapshot, err := snapshotFile(path)
	if err != nil {
		return stateMutation{}, err
	}
	return stateMutation{before: snapshot, delete: true}, nil
}

func snapshotFile(path string) (fileSnapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{path: path}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("snapshot %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("stat snapshot %s: %w", path, err)
	}
	return fileSnapshot{path: path, data: data, mode: info.Mode().Perm(), exists: true}, nil
}

func applyStateMutations(mutations []stateMutation) error {
	for i := range mutations {
		mutation := &mutations[i]
		var err error
		if mutation.delete {
			err = os.Remove(mutation.before.path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else {
			err = writeStateFile(mutation.before.path, mutation.data, mutation.mode)
		}
		if err != nil {
			rollbackErr := rollbackStateMutations(mutations[:i+1])
			return errors.Join(fmt.Errorf("mutate %s: %w", mutation.before.path, err), rollbackErr)
		}
	}
	return nil
}

func rollbackStateMutations(mutations []stateMutation) error {
	var rollbackErr error
	for i := len(mutations) - 1; i >= 0; i-- {
		snapshot := &mutations[i].before
		var err error
		if snapshot.exists {
			err = atomicWritePerm(snapshot.path, snapshot.data, snapshot.mode)
		} else {
			err = os.Remove(snapshot.path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback %s: %w", snapshot.path, err))
		}
	}
	return rollbackErr
}
