package mcpfmt

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/promote"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestCheckReadOnly(t *testing.T) {
	if err := CheckReadOnly([]mcp.Tool{FetchTool("u", ""), PublishTool("u", "")}); err != nil {
		t.Fatalf("correct annotations rejected: %v", err)
	}
	err := CheckReadOnly([]mcp.Tool{mcp.NewTool("mark_list"), mcp.NewTool("mark_publish", ReadOnly())})
	if err == nil || !strings.Contains(err.Error(), "mark_list, mark_publish") {
		t.Fatalf("err = %v, want both tools named", err)
	}
}

func TestPromoteArgsNameOnlyServedPhases(t *testing.T) {
	tests := []struct {
		name   string
		phases []string
		want   map[string]string // argument -> description; absent ones not advertised
	}{
		{name: "soul", phases: promote.SoulPhases, want: map[string]string{
			"body":   "refresh: the live knowledge body",
			"target": "stamp, refresh: the knowledge copy as <url>@v<N>",
			"via":    "stamp: the knowledge server's name, recorded as promoted-via",
			"stub":   "stamp: reduce the body to its title and summary (default false: keep it)",
			"packet": "stamp, refresh: the packet hash from the report before it",
		}},
		{name: "knowledge", phases: promote.KnowledgePhases, want: map[string]string{
			"body":     "dry_run, apply: the distilled draft",
			"metadata": "dry_run, apply: the draft's metadata object (tags, importance, type, the policy's axes)",
			"source":   "dry_run, apply: origin recorded as promoted-from (default none)",
			"packet":   "apply: the packet hash from the report before it",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props := PromoteTool("u", "", tt.phases).InputSchema.Properties
			for _, fixed := range []string{"url", "phase"} {
				delete(props, fixed)
			}
			if len(props) != len(tt.want) {
				t.Errorf("advertised %d arguments, want %d: %v", len(props), len(tt.want), props)
			}
			for arg, want := range tt.want {
				prop, _ := props[arg].(map[string]any)
				if got, _ := prop["description"].(string); got != want {
					t.Errorf("%s = %q, want %q", arg, got, want)
				}
			}
		})
	}
}
