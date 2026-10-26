package gateway

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
)

func TestMemoryGatewayServesRenderedPrompts(t *testing.T) {
	g := newMemoryGateway(t, mcpTestConfig(), seededDispatcher())
	listed := g.mcpServer.ListPrompts()
	// Every embedded file is listed; LoadPrompts fails on a file it skips.
	prompts, err := mcpfmt.LoadPrompts(memoryPromptFS, "prompts/memory/commands")
	if err != nil || len(listed) != len(prompts) {
		t.Fatalf("listed %d of %d prompts: %v", len(listed), len(prompts), err)
	}
	for _, p := range prompts {
		if listed[p.Prompt.Name] == nil {
			t.Errorf("prompts/list missing %q", p.Prompt.Name)
		}
	}
	text := mcpfmttest.PromptText(t, listed["soul-journal"].Handler, map[string]string{mcpfmt.PromptArgument: "shipped"})
	if !strings.Contains(text, "`mark://<w>/` for the root project") || !strings.HasSuffix(text, "Entry: shipped\n") {
		t.Errorf("soul-journal does not address the world or fill its entry:\n%s", text)
	}
}
