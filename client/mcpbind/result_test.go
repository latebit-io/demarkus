package mcpbind

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/mark3labs/mcp-go/mcp"
)

// Every tool answer reveals hidden text, errors included: stored versions
// may predate the write gate.
func TestResultRevealsHiddenText(t *testing.T) {
	for _, r := range []marktools.Result{
		{Text: "title: a\U0000202Eb\n\nhi \U000E0049\U000E0047\x1b[2J"},
		{Text: "failed: \x1b]0;x\x07", IsError: true},
	} {
		got := Result(r).Content[0].(mcp.TextContent).Text
		if strings.ContainsAny(got, "\x1b\x07\U0000202E\U000E0049") || !strings.Contains(got, "<U+") {
			t.Errorf("Result(%q) = %q", r.Text, got)
		}
	}
	if got := Text("clean").Content[0].(mcp.TextContent).Text; got != "clean" {
		t.Errorf("Text(clean) = %q", got)
	}
}
