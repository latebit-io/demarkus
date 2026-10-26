package gateway

import (
	"maps"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

func TestMemoryGatewayServesRenderedPrompts(t *testing.T) {
	g := newMemoryGateway(t, mcpTestConfig(), seededDispatcher())
	listed := g.mcpServer.ListPrompts()
	got := map[string]string{}
	for name, p := range listed {
		args := make([]string, 0, len(p.Prompt.Arguments))
		for _, a := range p.Prompt.Arguments {
			args = append(args, a.Name)
		}
		got[name] = strings.Join(args, ",")
	}
	arg := mcpfmt.PromptArgument
	want := map[string]string{"promote": arg, "promote-scan": arg, "soul-refresh": arg, "soul": "", "soul-archive": arg, "soul-context": "", "soul-curate": arg, "soul-doctor": arg, "soul-journal": arg, "soul-export": "directory"}
	if !maps.Equal(got, want) {
		t.Fatalf("prompts = %v, want %v", got, want)
	}
	text := mcpfmttest.PromptText(t, listed["soul-journal"].Handler, map[string]string{mcpfmt.PromptArgument: "shipped"})
	if !strings.Contains(text, "`mark://<w>/` for the root project") || !strings.HasSuffix(text, "Entry: shipped\n") {
		t.Errorf("soul-journal does not address the world or fill its entry:\n%s", text)
	}
}
