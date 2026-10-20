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

// Register appends SLUG to the knowledge-system registry (idempotent).
func Register(slug string) error {
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
		if slices.Contains(rows, slug) {
			return nil // already registered
		}
		if err := catalog.RejectLocalName(slug); err != nil {
			return err
		}
		rows = append(rows, slug)
		return statefile.Write(p, []byte(strings.Join(rows, "\n")+"\n"))
	})
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
