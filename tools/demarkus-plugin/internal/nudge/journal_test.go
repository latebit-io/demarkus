package nudge

import (
	"encoding/json"
	"testing"
)

func TestJournalWriteCall(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"route today", `{"tool_name":"mark_append","mcp_server_name":"demarkus-memory","tool_input":{"url":"/","route":"today"}}`, true},
		{"journal path", `{"tool_name":"mark_publish","mcp_server_name":"demarkus-memory","tool_input":{"url":"/p/journal/2026-10-14.md"}}`, true},
		{"escaped quote in url", `{"tool_name":"mark_publish","tool_input":{"url":"/p/a\"b/journal/x.md"}}`, true},
		{"pi mcp proxy", `{"tool":"mcp","input":{"tool":"soul_mark_append","args":"{\"url\":\"/\",\"route\":\"today\"}"}}`, true},
		{"plan whose body quotes a journal url", `{"tool_name":"mark_publish","tool_input":{"url":"/plans/x.md","body":"\"url\": \"/x/journal/a.md\""}}`, false},
		{"route lookalike", `{"tool_name":"mark_append","tool_input":{"url":"/","myroute":"today","route":"today-x"}}`, false},
		{"fetch of a journal", `{"tool_name":"mark_fetch","tool_input":{"url":"/journal/a.md"}}`, false},
		{"no tool_input", `{"tool_name":"mark_append"}`, false},
	}
	for _, c := range cases {
		var in Input
		if err := json.Unmarshal([]byte(c.payload), &in); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := JournalWriteCall(&in); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
