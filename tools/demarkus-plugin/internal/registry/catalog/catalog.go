// Package catalog reads and binds the remote memory catalog: rows, endpoints,
// project bindings and the local memory's alias. Joins that add rows live in
// package join; this package rejects the names they may not take.
package catalog

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// RemoteRow returns the catalog row for SLUG.
// ok is false when the slug isn't registered.
func RemoteRow(slug string) (config.MemoryRow, bool, error) {
	rows, err := config.RemoteMemoryRows()
	if err != nil {
		return config.MemoryRow{}, false, err
	}
	for _, row := range rows {
		if row.Slug == slug {
			if row.Host == "" {
				return config.MemoryRow{}, false, fmt.Errorf("memories catalog row for %q has no host; re-run /soul-join", slug)
			}
			return row, true, nil
		}
	}
	return config.MemoryRow{}, false, nil
}

// Endpoint is how a catalog store is reached: the local managed server or a
// remote row. Token is the file's trimmed content, "" when the row has none.
type Endpoint struct {
	Host     string // mark://host[:port]
	Insecure bool
	Broker   bool // HTTPS broker: OAuth in the harness, no token file
	Token    string
}

// Resolve resolves a catalog id. The local store needs its config and
// tolerates a missing token file (session start may not have run yet); a
// remote row without a readable token is an error unless it declares none.
func Resolve(id string) (Endpoint, error) {
	if id == config.LocalMemoryID {
		cfg, err := config.LoadConfig()
		if err != nil {
			return Endpoint{}, err
		}
		if cfg == nil {
			return Endpoint{}, errors.New("no local store configured; run /soul-init")
		}
		tf, err := config.TokenPath()
		if err != nil {
			return Endpoint{}, err
		}
		tok, err := os.ReadFile(tf)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Endpoint{}, fmt.Errorf("read plugin token: %w", err)
		}
		return Endpoint{Host: "mark://localhost:" + cfg.Port, Insecure: true, Token: strings.TrimSpace(string(tok))}, nil
	}
	row, ok, err := RemoteRow(id)
	if err != nil {
		return Endpoint{}, err
	}
	if !ok {
		return Endpoint{}, errors.New("store '" + id + "' is not in the catalog; run /soul-join")
	}
	ep := Endpoint{Host: row.Host, Insecure: row.Insecure, Broker: row.IsBroker()}
	if row.TokenFile == "" || row.TokenFile == "-" || ep.Broker {
		return ep, nil
	}
	tok, err := os.ReadFile(row.TokenFile)
	if err != nil {
		return Endpoint{}, fmt.Errorf("read token file %s: %w (re-run /soul-join --token if it is gone)", row.TokenFile, err)
	}
	if strings.TrimSpace(string(tok)) == "" {
		return Endpoint{}, errors.New("token file " + row.TokenFile + " is empty; re-run /soul-join --token")
	}
	ep.Token = strings.TrimSpace(string(tok))
	return ep, nil
}

// Listing emits one row per bindable memory: "<id>\t<tier>\t<host>\t<insecure>".
func Listing() ([]string, error) {
	var out []string
	local, err := config.LocalMemoryPresent()
	if err != nil {
		return nil, err
	}
	if local {
		out = append(out, "demarkus-memory\tlocal\t-\t-")
	}
	rows, err := config.RemoteMemoryRows()
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		host, ins := row.Host, "0"
		if host == "" {
			host = "-"
		}
		if row.Insecure {
			ins = "1"
		}
		// Broker memories register via HTTP MCP, not mcp-serve; the tier
		// column is how pickers show that difference.
		tier := "remote"
		if row.IsBroker() {
			tier = "broker"
		}
		out = append(out, fmt.Sprintf("%s\t%s\t%s\t%s", row.Slug, tier, host, ins))
	}
	return out, nil
}

// IsMemory reports whether slug is a bindable write target.
func IsMemory(slug string) (bool, error) {
	if slug == "" {
		return false, nil
	}
	if slug == config.LocalMemoryID {
		return config.LocalMemoryPresent()
	}
	remotes, err := config.ListRemoteMemories()
	if err != nil {
		return false, err
	}
	return slices.Contains(remotes, slug), nil
}

// BindProject records that DIR writes to catalog memory SLUG by default. Locked
// + atomic; validates SLUG is a joined memory first.
func BindProject(dir, slug string) error {
	memoriesPath, err := config.StatePath("souls")
	if err != nil {
		return err
	}
	bindingsPath, err := config.StatePath("project-souls")
	if err != nil {
		return err
	}
	return statefile.WithLock(memoriesPath, func() error {
		return statefile.WithLock(bindingsPath, func() error {
			ok, err := IsMemory(slug)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("'%s' is not a joined memory; run /soul-join first (see /soul-default --list)", slug)
			}
			mutation, err := BindingMutation(dir, slug, bindingsPath)
			if err != nil {
				return err
			}
			return statefile.Apply([]statefile.Mutation{mutation})
		})
	})
}

var genericLabels = map[string]bool{"mcp": true, "broker": true, "api": true, "gateway": true, "gw": true, "www": true}

// DeriveSlug lowercases HOST and returns the first non-generic DNS label, or the
// leftmost label if all are generic; then sanitizes to [a-z0-9-].
func DeriveSlug(host string) string {
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

// aliasSafe mirrors pluginNameRE in tools/plugin-prompts: the generator
// validated the key; this guards a hand-edited .mcp.json.
var aliasSafe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// SetLocalAlias records the MCP server name a branded plugin serves the
// local memory under (empty or the default id: the default name), returning the
// replaced alias and this write's token for RestoreLocalAlias.
func SetLocalAlias(alias string) (previous, token string, err error) {
	if alias == config.LocalMemoryID {
		alias = ""
	}
	return setLocalMemoryAlias(alias, "")
}

// RestoreLocalAlias puts previous back only while token still identifies
// the stored write, so a failed command never undoes a later command's record.
func RestoreLocalAlias(previous, token string) error {
	_, _, err := setLocalMemoryAlias(previous, token)
	return err
}

func newWriteToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// setLocalMemoryAlias writes "<token> <alias>" and returns the alias it
// replaced; a non-empty onlyIfToken makes the write conditional on that token
// still being the stored one.
func setLocalMemoryAlias(alias, onlyIfToken string) (previous, token string, err error) {
	p, err := config.StatePath(config.LocalMemoryAliasFile)
	if err != nil {
		return "", "", err
	}
	memoriesPath, err := config.StatePath("souls")
	if err != nil {
		return "", "", err
	}
	systemsPath, err := config.StatePath("knowledge-systems")
	if err != nil {
		return "", "", err
	}
	if alias != "" && !aliasSafe.MatchString(alias) {
		return "", "", fmt.Errorf("local memory alias '%s': lowercase letters, digits, and hyphens only", alias)
	}
	token, err = newWriteToken()
	if err != nil {
		return "", "", err
	}
	// Under both catalog locks (memories, then knowledge) so a concurrent join
	// or register cannot commit the same name; joins take memories first too.
	err = statefile.WithLock(memoriesPath, func() error {
		return statefile.WithLock(systemsPath, func() error {
			current, currentToken, err := config.LocalMemoryAliasRecord()
			if err != nil {
				return err
			}
			if onlyIfToken != "" && currentToken != onlyIfToken {
				return nil
			}
			previous = current
			if alias != "" {
				for _, list := range []func() ([]string, error){config.ListRemoteMemories, config.ListKnowledgeSystems} {
					slugs, err := list()
					if err != nil {
						return err
					}
					for _, slug := range slugs {
						if !config.ServerMatches(slug, alias) {
							continue
						}
						taken := fmt.Errorf("local memory alias '%s' would capture the joined store '%s'", alias, slug)
						if onlyIfToken == "" {
							return taken
						}
						// A restore whose value was joined meanwhile must still retire
						// our own record: no server runs under it.
						if err := statefile.Write(p, []byte(token+" \n")); err != nil {
							return err
						}
						return fmt.Errorf("alias cleared instead: %w", taken)
					}
				}
			}
			return statefile.Write(p, []byte(token+" "+alias+"\n"))
		})
	})
	return previous, token, err
}

// RejectLocalName fails a catalog insert whose slug would route to the
// local memory; called under the catalog lock so an alias write cannot interleave.
func RejectLocalName(slug string) error {
	local, err := config.ResolvesToLocalMemory(slug)
	if err != nil {
		return err
	}
	if local {
		return fmt.Errorf("slug '%s' is reserved for the local managed memory; join a host with a different first label", slug)
	}
	return nil
}

// BindingMutation prepares the bindings file with dir bound to slug, other rows kept.
func BindingMutation(dir, slug, path string) (statefile.Mutation, error) {
	if err := statefile.ValidateField("project binding directory", dir); err != nil {
		return statefile.Mutation{}, err
	}
	bindings, err := config.Records("project-souls")
	if err != nil {
		return statefile.Mutation{}, err
	}
	bound := make([]string, 0, len(bindings)+1)
	for _, row := range bindings {
		if strings.SplitN(row, "\t", 2)[0] != dir {
			bound = append(bound, row)
		}
	}
	bound = append(bound, dir+"\t"+slug)
	return statefile.Put(path, []byte(strings.Join(bound, "\n")+"\n"), 0o644)
}
