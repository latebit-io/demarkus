package mcpfmt

import (
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/promote"
)

func TestNewToolHintsFollowClass(t *testing.T) {
	read := NewTool("mark_fetch").Annotations
	if read.ReadOnlyHint == nil || !*read.ReadOnlyHint || read.DestructiveHint == nil || *read.DestructiveHint {
		t.Errorf("mark_fetch hints = %+v, want read-only and not destructive", read)
	}
	if write := NewTool("mark_publish").Annotations; write.ReadOnlyHint != nil && *write.ReadOnlyHint {
		t.Errorf("mark_publish claims read-only: %+v", write)
	}
	defer func() {
		if recover() == nil {
			t.Error("unclassified tool built without a panic")
		}
	}()
	NewTool("mark_unclassified")
}

// A promote argument is advertised only when a served phase reads it, and
// its description names served phases only.
func TestPromoteArgsNameOnlyServedPhases(t *testing.T) {
	for _, served := range [][]string{promote.SoulPhases, promote.KnowledgePhases, promote.AllPhases} {
		props := PromoteTool("u", "", served).InputSchema.Properties
		for _, arg := range promoteArgs {
			prop, advertised := props[arg.name].(map[string]any)
			readers := slices.ContainsFunc(arg.clauses, func(c phaseClause) bool {
				return slices.ContainsFunc(c.phases, func(p string) bool { return slices.Contains(served, p) })
			})
			if advertised != readers {
				t.Errorf("phases %v: %s advertised = %v, want %v", served, arg.name, advertised, readers)
				continue
			}
			desc, _ := prop["description"].(string)
			for clause := range strings.SplitSeq(desc, "; ") {
				names, _, _ := strings.Cut(clause, ": ")
				for name := range strings.SplitSeq(names, ", ") {
					if advertised && !slices.Contains(served, name) {
						t.Errorf("phases %v: %s names unserved phase %q", served, arg.name, name)
					}
				}
			}
		}
	}
}
