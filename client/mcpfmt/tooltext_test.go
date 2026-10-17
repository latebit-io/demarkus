package mcpfmt

import (
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/promote"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestBudgetReadsTokensAsBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want int
	}{
		{"absent", map[string]any{}, 0},
		{"zero", map[string]any{"budget": 0}, 0},
		{"negative", map[string]any{"budget": -5}, 0},
		{"tokens", map[string]any{"budget": 1500}, 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Arguments = tc.args
			if got := Budget(&req); got != tc.want {
				t.Fatalf("Budget = %d, want %d", got, tc.want)
			}
		})
	}
}

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
