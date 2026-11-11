package guidance

import (
	"cmp"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
)

// ResolveProject resolves dir, or the directory the harness exported for its
// hooks when dir is empty: the fallback is the surface's, not the resolver's.
func ResolveProject(dir string) (project.Resolution, error) {
	r, err := project.Resolve(cmp.Or(dir, host.ProjectDir()))
	if errors.Is(err, project.ErrNoDir) {
		return r, fmt.Errorf("%w: pass --dir or set the harness project variable", err)
	}
	return r, err
}

// Hint names the recovery for a binding the catalog no longer lists, so
// prompts relay it instead of restating which command restores which store.
func Hint(r project.Resolution) string {
	restore := "/soul-join"
	if r.Store == config.LocalMemoryID {
		restore = "/soul-init"
	}
	return "not in the catalog; restore it with " + restore + " or rebind with /soul-default"
}

// header renders the resolution as the session-start line; root is the
// project's directory in the store.
func header(r project.Resolution, root string) string {
	store := "Bound store: `" + r.Store + "`."
	switch r.State {
	case project.StateLocal:
		store = "Bound store: `" + r.Store + "` (local, no project binding)."
	case project.StateStale:
		store = "Bound store: `" + r.Store + "` (stale: " + Hint(r) + ")."
	}
	return "Project slug: `" + r.Slug + "`. Project root: `" + root + "`. " + store
}
