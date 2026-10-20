// Package knowledge keeps the joined knowledge system registry the publish
// gate scopes on.
package knowledge

import (
	"fmt"
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

// Register appends SLUG to the knowledge-system registry (idempotent) and
// records its broker URL when given, replacing an earlier one.
func Register(slug, url string) error {
	if err := checkSlug(slug); err != nil {
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
		if !slices.Contains(rows, slug) {
			if err := catalog.RejectLocalName(slug); err != nil {
				return err
			}
			rows = append(rows, slug)
			if err := statefile.Write(p, []byte(strings.Join(rows, "\n")+"\n")); err != nil {
				return err
			}
		}
		if url == "" {
			return nil
		}
		return writeURL(slug, url)
	})
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

// writeURL replaces slug's row in the url registry; url "" drops it.
// Callers hold the knowledge-systems lock.
func writeURL(slug, url string) error {
	p, err := config.StatePath(urlsFile)
	if err != nil {
		return err
	}
	rows, err := config.Records(urlsFile)
	if err != nil {
		return err
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
	if len(kept) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return statefile.Write(p, []byte(strings.Join(kept, "\n")+"\n"))
}

// Unregister drops the slug from the registry, so the publish gate stops
// enforcing on it, and clears the policy files an older helper mirrored.
// Idempotent; existed reports whether the slug was registered.
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
		if err := writeURL(slug, ""); err != nil {
			return err
		}
		if len(kept) == 0 {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else if err := statefile.Write(p, []byte(strings.Join(kept, "\n")+"\n")); err != nil {
			return err
		}
		return clearPolicyMirror(slug)
	})
	return existed, err
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
