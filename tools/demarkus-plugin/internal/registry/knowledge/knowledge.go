// Package knowledge keeps the joined knowledge system registry and the
// per-slug policy mirror the publish gate enforces.
package knowledge

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
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
// enforcing on it, and clears its mirrored policy files. Idempotent; existed
// reports whether the slug was registered.
func Unregister(slug string) (existed bool, err error) {
	if err := checkSlug(slug); err != nil {
		return false, err
	}
	p, err := config.StatePath("knowledge-systems")
	if err != nil {
		return false, err
	}
	// One lock spans the registry write and the policy cleanup, and MirrorPolicy
	// takes the same lock, so a concurrent re-mirror cannot leave orphaned
	// policy files the gate would keep enforcing.
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

// MirrorPolicy mirrors a knowledge system's enforceable policy to per-slug files.
// Missing directives clear their files. The registry lock serializes unregisters.
func MirrorPolicy(slug, body string) error {
	if err := checkSlug(slug); err != nil {
		return err
	}
	lockPath, err := config.StatePath("knowledge-systems")
	if err != nil {
		return err
	}
	return statefile.WithLock(lockPath, func() error {
		// A queued policy-mirror could run AFTER a knowledge-unregister; under the
		// shared lock, re-confirm the slug is still registered before writing, so
		// we don't resurrect policy files for a system that's no longer joined.
		rows, err := config.Records("knowledge-systems")
		if err != nil {
			return err
		}
		if !slices.Contains(rows, slug) {
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
		snapshot, err := statefile.Put(snapshotPath, policySnapshot(policy, values), 0o644)
		if err != nil {
			return err
		}
		var mutations []statefile.Mutation
		for _, key := range policyKeys {
			val := values[key]
			p, err := config.StatePath(fileFor[key])
			if err != nil {
				return err
			}
			var mutation statefile.Mutation
			if val == "" {
				mutation, err = statefile.Delete(p)
				if err != nil {
					return err
				}
			} else {
				mutation, err = statefile.Put(p, []byte(val+"\n"), 0o644)
				if err != nil {
					return err
				}
			}
			mutations = append(mutations, mutation)
		}
		mutations = append(mutations, snapshot)
		return statefile.Apply(mutations)
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
