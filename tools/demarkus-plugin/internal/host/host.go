// Package host names the harnesses the plugin serves and what differs between
// them at the output edge: hook payload shapes, the project directory
// variable, the MCP config file. The tool name grammar stays with config.
package host

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// gate phases of the two Claude Code gate hooks; Cursor has one pre-call hook.
const (
	gatePre  = "pre"
	gatePost = "post"
)

// Host is one harness's contract with the plugin binary. A nil shape means
// the harness has no such channel and reads the native JSON instead.
type Host struct {
	Name string
	// projectDirEnv is the variable the harness exports for its hooks; ""
	// when the adapter passes --project-dir.
	projectDirEnv string
	// NudgeOnce: Stop fires at every turn end and the nudge blocks a turn, so
	// the session-end nudge fires once per session.
	NudgeOnce bool
	// mcpConfig is the MCP config file under home; "" when the harness
	// registers servers itself (claude mcp add; opencode.json on load).
	mcpConfig string
	// httpAuthMarker: pi-mcp-adapter needs an explicit auth key on a remote
	// server entry; Cursor discovers OAuth from the endpoint.
	httpAuthMarker bool
	nudge          func(event, text string) any
	guidance       func(text string) any
	gate           func(phase, decision, reason string) (any, bool)
}

// The harnesses. Claude Code and Cursor run bash hooks and read a per-hook
// payload shape; pi and OpenCode adapters read the native JSON.
var (
	Claude = Host{
		Name: "claude", projectDirEnv: "CLAUDE_PROJECT_DIR", NudgeOnce: true,
		nudge:    claudeNudge,
		guidance: func(text string) any { return hookOutput("SessionStart", map[string]any{"additionalContext": text}) },
		gate:     claudeGate,
	}
	Cursor = Host{
		Name: "cursor", projectDirEnv: "CURSOR_PROJECT_DIR",
		mcpConfig: filepath.Join(".cursor", "mcp.json"),
		nudge:     cursorNudge,
		guidance:  func(text string) any { return map[string]any{"additional_context": text} },
		gate:      cursorGate,
	}
	OpenCode = Host{Name: "opencode"}
	Pi       = Host{Name: "pi", mcpConfig: filepath.Join(".config", "mcp", "mcp.json"), httpAuthMarker: true}
)

// hookFormats are the nudge and guidance --format values naming a payload
// shape; gateFormats the gate's, where Claude's phase is part of the name.
var (
	hookFormats = map[string]Host{"claude": Claude, "cursor": Cursor}
	gateFormats = map[string]struct {
		host  Host
		phase string
	}{
		"claude-pre":  {Claude, gatePre},
		"claude-post": {Claude, gatePost},
		"cursor":      {Cursor, ""},
	}
)

// ForHook resolves a nudge or guidance --format value; ok is false for json,
// the native shape.
func ForHook(format string) (Host, bool) {
	h, ok := hookFormats[format]
	return h, ok
}

// ForGate resolves a gate --format value to the host and phase answering.
func ForGate(format string) (h Host, phase string, ok bool) {
	g, ok := gateFormats[format]
	return g.host, g.phase, ok
}

// ForMcp resolves the --harness value of `registry mcp`: "" and "pi" edit
// pi-mcp-adapter's file, "cursor" Cursor's.
func ForMcp(name string) (Host, error) {
	switch name {
	case "", "pi":
		return Pi, nil
	case "cursor":
		return Cursor, nil
	}
	return Host{}, errors.New("unknown MCP harness '" + name + "' (use pi or cursor)")
}

// ProjectDir is the project directory a harness exported for its hooks, or "".
func ProjectDir() string {
	for _, h := range []Host{Cursor, Claude} {
		if v := os.Getenv(h.projectDirEnv); v != "" {
			return v
		}
	}
	return ""
}

// McpConfigPath is the MCP config file the mcp commands edit for h.
func (h Host) McpConfigPath() (string, error) {
	if h.mcpConfig == "" {
		return "", errors.New("host " + h.Name + " registers MCP servers itself")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, h.mcpConfig), nil
}

// HTTPEntry is h's config shape for a remote MCP server.
func (h Host) HTTPEntry(url string) map[string]any {
	if h.httpAuthMarker {
		return map[string]any{"url": url, "auth": "oauth"}
	}
	return map[string]any{"url": url}
}

// Nudge wraps a nudge for the hook that raised event.
func (h Host) Nudge(event, text string) any { return h.nudge(event, text) }

// Guidance wraps the session-start context.
func (h Host) Guidance(text string) any { return h.guidance(text) }

// Gate wraps a gate decision for phase; emit is false when this hook says nothing.
func (h Host) Gate(phase, decision, reason string) (out any, emit bool) {
	return h.gate(phase, decision, reason)
}

// hookOutput is Claude Code's envelope for hook output fields.
func hookOutput(event string, fields map[string]any) map[string]any {
	fields["hookEventName"] = event
	return map[string]any{"hookSpecificOutput": fields}
}

func claudeNudge(event, text string) any {
	switch event {
	case "session-end":
		return map[string]any{"decision": "block", "reason": text}
	case "promote":
		return hookOutput("PostToolUse", map[string]any{"additionalContext": text})
	default: // recall
		return hookOutput("UserPromptSubmit", map[string]any{"additionalContext": text})
	}
}

func cursorNudge(event, text string) any {
	switch event {
	case "session-end":
		return map[string]any{"followup_message": text}
	default: // promote (recall has no injection channel on Cursor)
		return map[string]any{"permission": "allow", "agent_message": text}
	}
}

// claudeGate: block and ask are enforced before the call; warn is deferred to
// PostToolUse as context.
func claudeGate(phase, decision, reason string) (any, bool) {
	switch phase {
	case gatePre:
		var pd string
		switch decision {
		case "block":
			pd = "deny"
		case "ask":
			pd = "ask"
		default:
			return nil, false // allow/warn → no Pre output
		}
		return hookOutput("PreToolUse", map[string]any{"permissionDecision": pd, "permissionDecisionReason": reason}), true
	default:
		if decision != "warn" {
			return nil, false // block/ask already handled Pre; allow → nothing
		}
		return hookOutput("PostToolUse", map[string]any{"additionalContext": "⚠️ " + reason}), true
	}
}

// cursorGate: one pre-call hook carries every verdict; warn is advice on an allow.
func cursorGate(_, decision, reason string) (any, bool) {
	switch decision {
	case "block":
		return map[string]any{"permission": "deny", "user_message": reason, "agent_message": reason}, true
	case "ask":
		return map[string]any{"permission": "ask", "user_message": reason, "agent_message": reason}, true
	case "warn":
		return map[string]any{"permission": "allow", "agent_message": "⚠️ " + reason}, true
	default:
		return map[string]any{"permission": "allow"}, true
	}
}

// NormalizeCall unwraps the pi-mcp-adapter "mcp" proxy: when the tool is the
// literal "mcp", the real tool name is input.tool and the real args are
// input.args (a JSON string or an object). Direct calls pass through unchanged.
func NormalizeCall(tool string, input map[string]any) (name string, args map[string]any) {
	if input == nil {
		input = map[string]any{}
	}
	if tool != "mcp" {
		return tool, input
	}
	t, ok := input["tool"].(string)
	if !ok || t == "" {
		return tool, input
	}
	switch raw := input["args"].(type) {
	case string:
		var parsed map[string]any
		if json.Unmarshal([]byte(raw), &parsed) == nil {
			return t, parsed
		}
		return t, map[string]any{}
	case map[string]any:
		return t, raw
	default:
		return t, map[string]any{}
	}
}
