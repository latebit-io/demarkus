// Package join registers a remote memory: a QUIC host with a token file or an
// HTTPS memory broker, as one rolled-back transaction over the catalog, the
// token file and the optional project binding.
package join

import (
	"fmt"
	"os"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/broker"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/client/joinurl"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Result is the outcome of a successful /soul-join.
type Result struct {
	Slug      string
	Host      string
	Insecure  bool
	TokenFile string
	// Broker marks an HTTPS memory-broker memory (OAuth at the endpoint,
	// registered with the harness's HTTP MCP transport, never mcp-serve);
	// McpURL is then the endpoint to register.
	Broker bool
	McpURL string
}

// Memory registers a remote memory: slug from host, token to a 0600 file,
// catalog row (slug collisions to another host rejected), optional binding.
// rawHost may be a join URL (mark://host#token=...) whose fragment supplies the token.
func Memory(rawHost, token string, insecure bool, bindDir string) (*Result, error) {
	h := strings.TrimSpace(rawHost)
	if config.IsBrokerHost(h) {
		return memoryJoinBroker(h, token, insecure, bindDir)
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
	slug := catalog.DeriveSlug(hostOnly)
	if slug == "" {
		return nil, fmt.Errorf("could not derive a slug from host '%s'", rawHost)
	}
	plan, err := planJoin(config.MemoryRow{Slug: slug, Host: "mark://" + h, Insecure: insecure}, token, bindDir)
	if err != nil {
		return nil, err
	}
	if err := plan.commit(); err != nil {
		return nil, err
	}
	return &Result{Slug: slug, Host: plan.row.Host, Insecure: insecure, TokenFile: plan.row.TokenFile}, nil
}

// memoryJoinBroker joins an HTTPS memory-broker memory: /knowledge-join's
// metadata validation, but the row lands in the memories catalog (gate +
// binding apply), no token file; OAuth happens in the MCP client.
func memoryJoinBroker(rawURL, token string, insecure bool, bindDir string) (*Result, error) {
	if token != "" {
		return nil, fmt.Errorf("a memory-broker memory authenticates via OAuth in the MCP client; do not pass a token")
	}
	if insecure {
		return nil, fmt.Errorf("--insecure applies only to self-signed QUIC memories; a memory broker is joined over verified TLS")
	}
	validated, err := broker.Validate(rawURL)
	if err != nil {
		return nil, err
	}
	plan, err := planJoin(config.MemoryRow{Slug: validated.Slug, Host: validated.URL}, "", bindDir)
	if err != nil {
		return nil, err
	}
	if err := plan.commit(); err != nil {
		return nil, err
	}
	return &Result{Slug: validated.Slug, Host: validated.URL, TokenFile: "-", Broker: true, McpURL: validated.McpURL}, nil
}

// joinPlan is everything one memory join commits: the catalog row (its
// TokenFile is the managed file or "-"), the token to store, the optional
// project binding, and the state files it locks.
type joinPlan struct {
	row              config.MemoryRow
	token            string // raw token to store; "" for a tokenless join
	managedTokenFile string
	bindDir          string
	memoriesPath     string
	bindingsPath     string // "" when no binding is requested
}

// planJoin resolves the state files a join touches; shared by the QUIC and
// broker paths. The reserved local slug is rejected at commit, under the lock.
func planJoin(row config.MemoryRow, token, bindDir string) (*joinPlan, error) {
	plan := &joinPlan{row: row, token: token, bindDir: bindDir}
	var err error
	if plan.memoriesPath, err = config.StatePath("souls"); err != nil {
		return nil, err
	}
	if bindDir != "" {
		if plan.bindingsPath, err = config.StatePath("project-souls"); err != nil {
			return nil, err
		}
	}
	if plan.managedTokenFile, err = config.StatePath("soul-" + row.Slug + ".token"); err != nil {
		return nil, err
	}
	plan.row.TokenFile = "-"
	if token != "" {
		plan.row.TokenFile = plan.managedTokenFile
	}
	return plan, nil
}

// commit applies the plan under its locks. Every multi-file registry path
// locks memories before project-memories; never reverse.
func (p *joinPlan) commit() error {
	mutate := func() error {
		mutations, replacedExternal, err := p.mutations()
		if err != nil {
			return err
		}
		if err := statefile.Apply(mutations); err != nil {
			return err
		}
		if replacedExternal != "" {
			if _, err := fmt.Fprintf(os.Stderr, "memory %s: catalog reference moved from %s to %s; the old external token file still exists\n", p.row.Slug, replacedExternal, p.managedTokenFile); err != nil {
				return fmt.Errorf("memory join committed but reporting replaced external token reference failed: %w", err)
			}
		}
		return nil
	}
	return statefile.WithLock(p.memoriesPath, func() error {
		if p.bindingsPath == "" {
			return mutate()
		}
		return statefile.WithLock(p.bindingsPath, mutate)
	})
}

// mutations prepares the ordered file writes and names the external token
// file whose catalog reference this join replaces, if any.
func (p *joinPlan) mutations() ([]statefile.Mutation, string, error) {
	if err := catalog.RejectLocalName(p.row.Slug); err != nil {
		return nil, "", err
	}
	existing, exists, err := catalog.RemoteRow(p.row.Slug)
	if err != nil {
		return nil, "", err
	}
	if exists && existing.Host != p.row.Host {
		return nil, "", fmt.Errorf("memory slug '%s' is already joined for host '%s'; joining '%s' would retarget every project bound to it; remove the existing entry or join from a host with a different first label", p.row.Slug, existing.Host, p.row.Host)
	}
	replacedExternal, err := p.replacedExternalToken(existing, exists)
	if err != nil {
		return nil, "", err
	}
	rows, err := config.Records("souls")
	if err != nil {
		return nil, "", err
	}
	kept := make([]string, 0, len(rows)+1)
	for _, row := range rows {
		if config.ParseMemoryRow(row).Slug != p.row.Slug {
			kept = append(kept, row)
		}
	}
	kept = append(kept, p.row.Record())

	mutations := make([]statefile.Mutation, 0, 3)
	var tokenDelete *statefile.Mutation
	if p.token != "" {
		mutation, err := statefile.Put(p.row.TokenFile, []byte(p.token), 0o600)
		if err != nil {
			return nil, "", err
		}
		mutations = append(mutations, mutation)
	} else {
		mutation, err := statefile.Delete(p.managedTokenFile)
		if err != nil {
			return nil, "", err
		}
		tokenDelete = &mutation
	}
	rowsWrite, err := statefile.Put(p.memoriesPath, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
	if err != nil {
		return nil, "", err
	}
	mutations = append(mutations, rowsWrite)
	// Publish the tokenless catalog row before removing the old token so readers
	// never observe a token-backed row whose credential file is already gone.
	if tokenDelete != nil {
		mutations = append(mutations, *tokenDelete)
	}
	if p.bindDir != "" {
		binding, err := catalog.BindingMutation(p.bindDir, p.row.Slug, p.bindingsPath)
		if err != nil {
			return nil, "", err
		}
		mutations = append(mutations, binding)
	}
	return mutations, replacedExternal, nil
}

func (p *joinPlan) replacedExternalToken(existing config.MemoryRow, exists bool) (string, error) {
	if !exists || existing.TokenFile == "" || existing.TokenFile == "-" || existing.TokenFile == p.managedTokenFile {
		return "", nil
	}
	if p.token == "" {
		return "", fmt.Errorf("memory '%s' uses externally managed token file %q; refusing to remove its catalog reference during tokenless rejoin; resolve that credential explicitly first", p.row.Slug, existing.TokenFile)
	}
	return existing.TokenFile, nil
}
