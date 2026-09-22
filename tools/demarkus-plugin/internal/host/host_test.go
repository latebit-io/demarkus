package host

import (
	"encoding/json"
	"testing"
)

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHookShapes pins every payload the bash hooks parse.
func TestHookShapes(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want string
	}{
		{"claude session-end nudge", Claude.Nudge("session-end", "n"), `{"decision":"block","reason":"n"}`},
		{"claude promote nudge", Claude.Nudge("promote", "n"), `{"hookSpecificOutput":{"additionalContext":"n","hookEventName":"PostToolUse"}}`},
		{"claude recall nudge", Claude.Nudge("recall", "n"), `{"hookSpecificOutput":{"additionalContext":"n","hookEventName":"UserPromptSubmit"}}`},
		{"cursor session-end nudge", Cursor.Nudge("session-end", "n"), `{"followup_message":"n"}`},
		{"cursor promote nudge", Cursor.Nudge("promote", "n"), `{"agent_message":"n","permission":"allow"}`},
		{"claude guidance", Claude.Guidance("g"), `{"hookSpecificOutput":{"additionalContext":"g","hookEventName":"SessionStart"}}`},
		{"cursor guidance", Cursor.Guidance("g"), `{"additional_context":"g"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encode(t, tt.got); got != tt.want {
				t.Errorf("got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestGateShapes(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		decision string
		want     string // "" means the hook says nothing
	}{
		{"claude pre block", "claude-pre", "block", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"r"}}`},
		{"claude pre ask", "claude-pre", "ask", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"r"}}`},
		{"claude pre warn", "claude-pre", "warn", ""},
		{"claude pre allow", "claude-pre", "allow", ""},
		{"claude post warn", "claude-post", "warn", `{"hookSpecificOutput":{"additionalContext":"⚠️ r","hookEventName":"PostToolUse"}}`},
		{"claude post block", "claude-post", "block", ""},
		{"claude post allow", "claude-post", "allow", ""},
		{"cursor block", "cursor", "block", `{"agent_message":"r","permission":"deny","user_message":"r"}`},
		{"cursor ask", "cursor", "ask", `{"agent_message":"r","permission":"ask","user_message":"r"}`},
		{"cursor warn", "cursor", "warn", `{"agent_message":"⚠️ r","permission":"allow"}`},
		{"cursor allow", "cursor", "allow", `{"permission":"allow"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, phase, ok := ForGate(tt.format)
			if !ok {
				t.Fatalf("ForGate(%q) not ok", tt.format)
			}
			out, emit := h.Gate(phase, tt.decision, "r")
			if emit != (tt.want != "") {
				t.Fatalf("emit = %v, want %v", emit, tt.want != "")
			}
			if emit {
				if got := encode(t, out); got != tt.want {
					t.Errorf("got %s\nwant %s", got, tt.want)
				}
			}
		})
	}
}

// TestFormats pins the format vocabularies: json and unknown values are the
// native shape, and gate names are not hook names.
func TestFormats(t *testing.T) {
	for _, f := range []string{"json", "", "claude-pre", "vim"} {
		if _, ok := ForHook(f); ok {
			t.Errorf("ForHook(%q) resolved a host", f)
		}
	}
	for _, f := range []string{"json", "", "claude", "vim"} {
		if _, _, ok := ForGate(f); ok {
			t.Errorf("ForGate(%q) resolved a host", f)
		}
	}
	if h, ok := ForHook("claude"); !ok || h.Name != "claude" || !h.NudgeOnce {
		t.Errorf("ForHook(claude) = %+v, %v", h, ok)
	}
	if h, ok := ForHook("cursor"); !ok || h.Name != "cursor" || h.NudgeOnce {
		t.Errorf("ForHook(cursor) = %+v, %v", h, ok)
	}
}

func TestForMcp(t *testing.T) {
	for name, want := range map[string]string{"": "pi", "pi": "pi", "cursor": "cursor"} {
		h, err := ForMcp(name)
		if err != nil || h.Name != want {
			t.Errorf("ForMcp(%q) = %s, %v; want %s", name, h.Name, err, want)
		}
	}
	if _, err := ForMcp("vim"); err == nil || err.Error() != "unknown MCP harness 'vim' (use pi or cursor)" {
		t.Errorf("ForMcp(vim) err = %v", err)
	}
	if _, err := Claude.McpConfigPath(); err == nil {
		t.Error("Claude has no MCP config file to edit")
	}
	if e := Pi.HTTPEntry("u"); e["auth"] != "oauth" {
		t.Errorf("pi entry = %v, want the auth marker", e)
	}
	if e := Cursor.HTTPEntry("u"); len(e) != 1 || e["url"] != "u" {
		t.Errorf("cursor entry = %v, want url only", e)
	}
}

func TestProjectDir(t *testing.T) {
	t.Setenv("CURSOR_PROJECT_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if got := ProjectDir(); got != "" {
		t.Errorf("ProjectDir() = %q with nothing exported", got)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "/c")
	t.Setenv("CURSOR_PROJECT_DIR", "/k")
	if got := ProjectDir(); got != "/k" {
		t.Errorf("ProjectDir() = %q, want Cursor's first", got)
	}
}
