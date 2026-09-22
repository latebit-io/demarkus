// Package project resolves the memory identity of a project directory: its
// slug (the path segment under the store root) and the store it writes to.
// The session-start header and the `registry project` subcommand both render
// it, so the prompts state neither the slug rule nor the binding lookup.
package project

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
)

// States of the resolved store binding.
const (
	StateLocal = "local" // no binding; the local managed store
	StateBound = "bound" // bound to a store present in the catalog
	StateStale = "stale" // bound to a store the catalog no longer lists
)

// ErrNoDir is returned when neither the caller nor the harness names a project directory.
var ErrNoDir = errors.New("no project directory: pass --dir or set the harness project variable")

// slugRe is the path segment alphabet; it excludes delimiters and dot-only names.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Resolution is a project's slug and bound store.
type Resolution struct {
	Dir   string // absolute project directory
	Slug  string // basename lowercased, spaces to hyphens
	Store string // catalog id of the store the project writes to
	State string // StateLocal, StateBound or StateStale
}

// Resolve derives the slug and binding for dir, falling back to the harness
// project directory when dir is empty. Bindings are recorded absolute, so a
// relative dir is made absolute before the lookup.
func Resolve(dir string) (Resolution, error) {
	if dir == "" {
		dir = host.ProjectDir()
	}
	if dir == "" {
		return Resolution{}, ErrNoDir
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve project dir: %w", err)
	}
	slug, err := Slug(dir)
	if err != nil {
		return Resolution{}, err
	}
	r := Resolution{Dir: dir, Slug: slug, Store: config.LocalMemoryID, State: StateLocal}
	bound, err := config.ProjectBinding(dir)
	if err != nil {
		return Resolution{}, err
	}
	if bound == "" {
		return r, nil
	}
	joined, err := catalog.IsMemory(bound)
	if err != nil {
		return Resolution{}, err
	}
	r.Store, r.State = bound, StateBound
	if !joined {
		r.State = StateStale
	}
	return r, nil
}

// SlugError reports a directory name that cannot be a store path segment.
type SlugError struct{ Name string }

func (e *SlugError) Error() string {
	return "directory name " + strconv.Quote(e.Name) + " is not a valid slug; ask the user which project"
}

// Slug is the store path segment for a project directory. A basename that
// cannot be a path segment is a SlugError rather than a guessed name.
func Slug(dir string) (string, error) {
	slug := strings.ReplaceAll(strings.ToLower(filepath.Base(dir)), " ", "-")
	if !IsSlug(slug) {
		return "", &SlugError{Name: filepath.Base(dir)}
	}
	return slug, nil
}

// IsSlug reports whether s can be a store path segment.
func IsSlug(s string) bool { return slugRe.MatchString(s) }

// Hint names the recovery for a binding the catalog no longer lists, so
// prompts relay it instead of restating which command restores which store.
func (r Resolution) Hint() string {
	restore := "/soul-join"
	if r.Store == config.LocalMemoryID {
		restore = "/soul-init"
	}
	return "not in the catalog; restore it with " + restore + " or rebind with /soul-default"
}

// Header renders the resolution as the session-start line.
func (r Resolution) Header() string {
	store := "Bound store: `" + r.Store + "`."
	switch r.State {
	case StateLocal:
		store = "Bound store: `" + r.Store + "` (local, no project binding)."
	case StateStale:
		store = "Bound store: `" + r.Store + "` (stale: " + r.Hint() + ")."
	}
	return "Project slug: `" + r.Slug + "`. " + store
}

// Lines renders the resolution as key=value lines for the registry subcommand;
// a stale binding adds a hint line with its recovery.
func (r Resolution) Lines() string {
	lines := "slug=" + r.Slug + "\nstore=" + r.Store + "\nstate=" + r.State
	if r.State == StateStale {
		lines += "\nhint=" + r.Hint()
	}
	return lines
}
