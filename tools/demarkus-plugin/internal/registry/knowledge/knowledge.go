// Package knowledge keeps the joined knowledge system registry the publish
// gate scopes on.
package knowledge

import (
	"fmt"
	neturl "net/url"
	"os"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// checkSlug rejects a slug that cannot name a state file.
func checkSlug(slug string) error {
	if !statefile.ValidSlug(slug) {
		return fmt.Errorf("invalid slug '%s': only [A-Za-z0-9._-] allowed", slug)
	}
	return nil
}

// urlsFile keeps `slug<TAB>url` rows beside the slug registry, which other
// adapters read as bare slugs; the session status probes the url.
const urlsFile = "knowledge-urls"

// checkURL rejects a url that would break a registry row or a probe.
func checkURL(slug, url string) error {
	if url == "" {
		return nil
	}
	parsed, err := neturl.Parse(url)
	if err != nil || strings.ContainsAny(url, "\t\r\n") || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("knowledge system '%s': broker URL %q is not an absolute URL", slug, url)
	}
	return nil
}

// Register adds SLUG and its broker URL to the registry in one atomic write.
// Idempotent for the same url; another url is refused, since every harness
// shares the registry and a silent replacement would strand their endpoints.
func Register(slug, url string) error {
	if err := checkSlug(slug); err != nil {
		return err
	}
	if err := checkURL(slug, url); err != nil {
		return err
	}
	p, err := config.StatePath("knowledge-systems")
	if err != nil {
		return err
	}
	return statefile.WithLock(p, func() error {
		rows, err := config.Records("knowledge-systems")
		if err != nil {
			return err
		}
		var mutations []statefile.Mutation
		if !slices.Contains(rows, slug) {
			if err := catalog.RejectLocalName(slug); err != nil {
				return err
			}
			m, err := statefile.Put(p, []byte(strings.Join(append(rows, slug), "\n")+"\n"), 0o644)
			if err != nil {
				return err
			}
			mutations = append(mutations, m)
		}
		if url != "" {
			current, err := URL(slug)
			if err != nil {
				return err
			}
			if current != "" && current != url {
				return fmt.Errorf("knowledge system '%s' is joined at %s; run `registry knowledge-unregister %s` before joining %s", slug, current, slug, url)
			}
			if current == "" {
				m, err := urlMutation(slug, url)
				if err != nil {
					return err
				}
				mutations = append(mutations, m)
			}
		}
		return statefile.Apply(mutations)
	})
}

// Unregister drops the slug and its url from the registry, so the publish
// gate stops enforcing on it, and clears the policy files an older helper
// mirrored. Idempotent; existed reports whether the slug was registered.
func Unregister(slug string) (existed bool, err error) {
	if err := checkSlug(slug); err != nil {
		return false, err
	}
	p, err := config.StatePath("knowledge-systems")
	if err != nil {
		return false, err
	}
	err = statefile.WithLock(p, func() error {
		rows, err := config.Records("knowledge-systems")
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
		systems, err := rowsMutation(p, kept)
		if err != nil {
			return err
		}
		urls, err := urlMutation(slug, "")
		if err != nil {
			return err
		}
		if err := statefile.Apply([]statefile.Mutation{systems, urls}); err != nil {
			return err
		}
		return clearPolicyMirror(slug)
	})
	return existed, err
}

// URL is the broker URL recorded for slug, "" when the join predates it.
// Callers need no lock: a torn read only costs one session's probe.
func URL(slug string) (string, error) {
	rows, err := config.Records(urlsFile)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if name, u, ok := strings.Cut(row, "\t"); ok && name == slug {
			return u, nil
		}
	}
	return "", nil
}

// Host is SLUG's promote identity, its broker URL's lowercased hostname; ""
// for a join older than the URL registry. OAuth's resource binding keeps it
// equal to the gateway's mark_worlds system: (its public URL's host).
func Host(slug string) (string, error) {
	raw, err := URL(slug)
	if err != nil || raw == "" {
		return "", err
	}
	u, err := neturl.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("knowledge system '%s': broker URL %q has no host", slug, raw)
	}
	return strings.ToLower(u.Hostname()), nil
}

// urlMutation stages slug's row in the url registry; url "" drops it.
// Callers hold the knowledge-systems lock.
func urlMutation(slug, url string) (statefile.Mutation, error) {
	p, err := config.StatePath(urlsFile)
	if err != nil {
		return statefile.Mutation{}, err
	}
	rows, err := config.Records(urlsFile)
	if err != nil {
		return statefile.Mutation{}, err
	}
	kept := rows[:0]
	for _, row := range rows {
		if name, _, _ := strings.Cut(row, "\t"); name != slug {
			kept = append(kept, row)
		}
	}
	if url != "" {
		kept = append(kept, slug+"\t"+url)
	}
	return rowsMutation(p, kept)
}

// rowsMutation stages a registry file's rows; none removes the file.
func rowsMutation(p string, rows []string) (statefile.Mutation, error) {
	if len(rows) == 0 {
		return statefile.Delete(p)
	}
	return statefile.Put(p, []byte(strings.Join(rows, "\n")+"\n"), 0o644)
}

// policyFiles are the per-slug mirror files helpers before 0.71 wrote; the
// server enforces the policy now, so they are only ever removed.
func policyFiles(slug string) []string {
	return []string{"plugin-knowledge.policy." + slug, "plugin-knowledge.strictness." + slug,
		"plugin-knowledge.require-tags." + slug, "plugin-knowledge.require-fields." + slug}
}

// ClearPolicyMirror removes the per-slug policy files an older helper wrote.
func ClearPolicyMirror(slug string) error {
	if err := checkSlug(slug); err != nil {
		return err
	}
	p, err := config.StatePath("knowledge-systems")
	if err != nil {
		return err
	}
	return statefile.WithLock(p, func() error { return clearPolicyMirror(slug) })
}

// clearPolicyMirror removes the per-slug policy files. Callers must already
// hold the knowledge-systems lock.
func clearPolicyMirror(slug string) error {
	for _, name := range policyFiles(slug) {
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
