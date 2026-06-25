package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/gate"
)

// captureStdout runs fn and returns whatever it wrote to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = orig
	b, _ := io.ReadAll(r)
	return strings.TrimSpace(string(b))
}

func TestEmitDecisionCodex(t *testing.T) {
	cases := []struct {
		name      string
		format    string
		dec       gate.Decision
		wantEmpty bool
		// expectations on the parsed hookSpecificOutput
		event      string
		permission string // for *-pre
		reasonHas  string
		ctxHas     string // for *-post / additionalContext
	}{
		{name: "codex-pre block→deny", format: "codex-pre", dec: gate.Decision{Decision: "block", Reason: "no tags"}, event: "PreToolUse", permission: "deny", reasonHas: "no tags"},
		{name: "codex-pre ask→deny+confirm", format: "codex-pre", dec: gate.Decision{Decision: "ask", Reason: "stricter."}, event: "PreToolUse", permission: "deny", reasonHas: "Confirm with the user"},
		{name: "codex-pre allow→empty", format: "codex-pre", dec: gate.Decision{Decision: "allow"}, wantEmpty: true},
		{name: "codex-pre warn→empty", format: "codex-pre", dec: gate.Decision{Decision: "warn", Reason: "x"}, wantEmpty: true},
		{name: "codex-post warn→ctx", format: "codex-post", dec: gate.Decision{Decision: "warn", Reason: "tag it"}, event: "PostToolUse", ctxHas: "tag it"},
		{name: "codex-post block→empty", format: "codex-post", dec: gate.Decision{Decision: "block", Reason: "x"}, wantEmpty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { emitDecision(tc.dec, tc.format) })
			if tc.wantEmpty {
				if out != "" {
					t.Fatalf("want no output, got %q", out)
				}
				return
			}
			var got struct {
				HookSpecificOutput struct {
					HookEventName            string `json:"hookEventName"`
					PermissionDecision       string `json:"permissionDecision"`
					PermissionDecisionReason string `json:"permissionDecisionReason"`
					AdditionalContext        string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("parse %q: %v", out, err)
			}
			h := got.HookSpecificOutput
			if h.HookEventName != tc.event {
				t.Errorf("event: want %q, got %q", tc.event, h.HookEventName)
			}
			if tc.permission != "" && h.PermissionDecision != tc.permission {
				t.Errorf("permissionDecision: want %q, got %q", tc.permission, h.PermissionDecision)
			}
			if tc.reasonHas != "" && !strings.Contains(h.PermissionDecisionReason, tc.reasonHas) {
				t.Errorf("reason %q missing %q", h.PermissionDecisionReason, tc.reasonHas)
			}
			if tc.ctxHas != "" && !strings.Contains(h.AdditionalContext, tc.ctxHas) {
				t.Errorf("additionalContext %q missing %q", h.AdditionalContext, tc.ctxHas)
			}
		})
	}
}
