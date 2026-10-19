package guidance

import "github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"

// Verdict is the one line a prompt acts on after registry project: use the
// store, or stop with the recovery, so no template restates the state table.
// An unprobed project is still usable; its root is the unconfirmed default.
func Verdict(r project.Resolution, st ProjectStatus) string {
	switch {
	case r.State == project.StateStale:
		return "stop: the bound store is " + r.Hint()
	case st.State == ProjectUnknown:
		return "use, root unconfirmed: " + st.Reason
	}
	return "use"
}
