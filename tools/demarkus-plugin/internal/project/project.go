// Package project resolves the memory identity of a project directory: its
// slug (the path segment under the store root) and the store it writes to.
// The surfaces render it (guidance), so no prompt states the slug rule or
// the binding lookup.
package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/memory/audit"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
)

// States of the resolved store binding.
const (
	StateLocal = "local" // no binding; the local managed store
	StateBound = "bound" // bound to a store present in the catalog
	StateStale = "stale" // bound to a store the catalog no longer lists
)

// ErrNoDir is returned when the caller names no project directory.
var ErrNoDir = errors.New("no project directory")

// Resolution is a project's slug and bound store.
type Resolution struct {
	Dir       string // absolute project directory; a worktree stays itself
	Slug      string
	Source    string // which rule named the slug (Source*)
	Linked    bool   // Dir is in a linked worktree
	Store     string // catalog id of the store the project writes to
	State     string // StateLocal, StateBound or StateStale
	Inherited bool   // the binding is the main checkout's, not Dir's own
}

// Resolve derives the slug and binding for dir. Bindings are recorded
// absolute, so a relative dir is made absolute before the lookup.
func Resolve(dir string) (Resolution, error) {
	dir, co, err := locate(dir)
	if err != nil {
		return Resolution{}, err
	}
	slug, source, err := identify(dir, co)
	if err != nil {
		return Resolution{}, err
	}
	r := Resolution{Dir: dir, Slug: slug, Source: source, Linked: co.linked, Store: config.LocalMemoryID, State: StateLocal}
	rows, err := config.ProjectBindings()
	if err != nil {
		return Resolution{}, err
	}
	bound, inherited := boundIn(rows, dir, co)
	if bound == "" {
		return r, nil
	}
	joined, err := catalog.IsMemory(bound)
	if err != nil {
		return Resolution{}, err
	}
	r.Store, r.State, r.Inherited = bound, StateBound, inherited
	if !joined {
		r.State = StateStale
	}
	return r, nil
}

// locate makes dir absolute and finds the checkout it sits in.
func locate(dir string) (abs string, co checkout, err error) {
	if dir == "" {
		return "", checkout{}, ErrNoDir
	}
	if abs, err = filepath.Abs(dir); err != nil {
		return "", checkout{}, fmt.Errorf("resolve project dir: %w", err)
	}
	co, err = findCheckout(abs)
	return abs, co, err
}

// SlugError reports a directory name that cannot be a store path segment.
type SlugError struct{ Name string }

func (e *SlugError) Error() string {
	return "directory name " + strconv.Quote(e.Name) + " is not a valid slug; ask the user which project"
}

// Slug is the store path segment a directory's own name gives. A basename
// that cannot be a path segment is a SlugError rather than a guessed name.
func Slug(dir string) (string, error) {
	slug := normalize(filepath.Base(dir))
	if !IsSlug(slug) {
		return "", &SlugError{Name: filepath.Base(dir)}
	}
	return slug, nil
}

func normalize(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "-")
}

// IsSlug reports whether s can be a store path segment; the audit owns the rule.
func IsSlug(s string) bool { return audit.IsSlug(s) }

// ByDirName is what the directory-name rule gave dir before git named
// projects: its basename, and the store bound to the directory as written
// ("" for none). Kept so a session can find the notes that rule left behind.
func ByDirName(dir string) (slug, store string, err error) {
	if slug, err = Slug(dir); err != nil {
		return "", "", err
	}
	rows, err := config.ProjectBindings()
	if err != nil {
		return "", "", err
	}
	store, _ = nearest(dir, "", rows.Lookup)
	return slug, store, nil
}

// BoundStore is the catalog id dir's writes are bound to, "" when unbound.
// A linked worktree without a row of its own takes its main checkout's
// (inherited), so every checkout of a repository writes to one store.
func BoundStore(dir string) (store string, inherited bool, err error) {
	if dir == "" {
		return "", false, nil
	}
	rows, err := config.ProjectBindings()
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	dir, co, err := locate(dir)
	if err != nil {
		return "", false, err
	}
	store, inherited = boundIn(rows, dir, co)
	return store, inherited, nil
}

// boundIn walks the rows for dir. A linked worktree's own rows come first,
// then the same path in the main checkout, then what encloses the worktree,
// then what encloses the main checkout: where a worktree sits is the user's choice.
func boundIn(rows config.Bindings, dir string, co checkout) (store string, inherited bool) {
	if len(rows) == 0 {
		return "", false
	}
	type step struct {
		from, stop string
		inherited  bool
	}
	steps := []step{{from: dir}}
	if co.linked {
		steps = []step{
			{dir, co.top, false},
			{mirror(co, dir), co.anchor, true},
			{filepath.Dir(co.top), "", false},
			{filepath.Dir(co.anchor), "", true},
		}
	}
	m := matcher{rows: rows}
	for _, s := range steps {
		if store, ok := nearest(s.from, s.stop, m.match); ok {
			return store, s.inherited
		}
	}
	return "", false
}

// nearest is the store match gives dir or its closest matched ancestor.
func nearest(dir, stop string, match func(string) (string, bool)) (string, bool) {
	for cur := range upTo(dir, stop) {
		if store, ok := match(cur); ok {
			return store, true
		}
	}
	return "", false
}

// mirror is dir's path inside the main checkout; top is an ancestor of dir.
func mirror(co checkout, dir string) string {
	return filepath.Join(co.anchor, strings.TrimPrefix(dir, co.top))
}

// matcher finds the row for a directory under any spelling: git records a
// real path, a harness the one the user typed (a symlinked home, /var).
type matcher struct {
	rows config.Bindings
	info []fs.FileInfo // rows' directories, nil where not on disk; read on first miss
}

func (m *matcher) match(p string) (string, bool) {
	if store, ok := m.rows.Lookup(p); ok {
		return store, true
	}
	if same := m.aliases(p); len(same) > 0 {
		return same[len(same)-1].Store, true
	}
	return "", false
}

// aliases are the rows naming p's directory, under any spelling, in file order.
func (m *matcher) aliases(p string) []config.Binding {
	at, err := os.Stat(p)
	if err != nil {
		return nil // not on disk: only its spelling could have matched
	}
	if m.info == nil {
		m.info = make([]fs.FileInfo, len(m.rows))
		for i, row := range m.rows {
			m.info[i] = statRow(row.Dir)
		}
	}
	var same []config.Binding
	for i, row := range m.rows {
		if m.info[i] != nil && os.SameFile(m.info[i], at) {
			same = append(same, row)
		}
	}
	return same
}

// statRow is a recorded directory on disk, nil when it is not: a row for a
// directory since removed is ordinary, any other failure is logged.
func statRow(dir string) fs.FileInfo {
	fi, err := os.Stat(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] project: binding row "+strconv.Quote(dir)+": "+err.Error())
		}
		return nil
	}
	return fi
}

// BindTarget is the bindings row a default set from dir writes: the same path
// in the main checkout for a linked worktree, so the repository has one
// binding. It replaces the worktree's own rows, which would outrank it.
func BindTarget(dir string) (catalog.BindTarget, error) {
	dir, co, err := locate(dir)
	if err != nil {
		return catalog.BindTarget{}, err
	}
	if !co.linked {
		return catalog.BindTarget{Dir: dir}, nil
	}
	rows, err := config.ProjectBindings()
	if err != nil {
		return catalog.BindTarget{}, err
	}
	target := catalog.BindTarget{Dir: mirror(co, dir), Replaces: slices.Collect(upTo(dir, co.top))}
	// One row per directory: another spelling of the target goes too.
	m := matcher{rows: rows}
	for _, row := range m.aliases(target.Dir) {
		if row.Dir != target.Dir {
			target.Replaces = append(target.Replaces, row.Dir)
		}
	}
	return target, nil
}
