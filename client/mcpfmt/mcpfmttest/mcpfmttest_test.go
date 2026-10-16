package mcpfmttest

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestRenderSortsAndKeepsHTMLLiteral(t *testing.T) {
	got, err := render([]mcp.Tool{
		mcp.NewTool("mark_b", mcp.WithDescription("journal/<YYYY-MM-DD>.md & more")),
		mcp.NewTool("mark_a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if !strings.Contains(body, "journal/<YYYY-MM-DD>.md & more") {
		t.Errorf("HTML characters escaped:\n%s", body)
	}
	if strings.Index(body, `"mark_a"`) > strings.Index(body, `"mark_b"`) {
		t.Errorf("tools not sorted by name:\n%s", body)
	}
	if !strings.HasSuffix(body, "]\n") {
		t.Errorf("missing trailing newline: %q", body[len(body)-3:])
	}
}

func TestFirstDiff(t *testing.T) {
	tests := []struct {
		name, want, got, msg string
	}{
		{name: "changed line", want: "a\nb\n", got: "a\nc\n", msg: `line 2: golden "b", served "c"`},
		{name: "added line", want: "a\n", got: "a\nb\n", msg: `line 2: golden "", served "b"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if msg := firstDiff([]byte(tt.want), []byte(tt.got)); msg != tt.msg {
				t.Fatalf("firstDiff = %s, want %s", msg, tt.msg)
			}
		})
	}
}
