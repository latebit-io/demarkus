package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// transcriptWith writes a one-line Claude transcript whose assistant turn
// called the named tool.
func transcriptWith(t *testing.T, tool string, input map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "name": tool, "input": input},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTaintGate(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n", "knowledge-systems": "knowledge\n"})
	web := transcriptWith(t, "WebFetch", map[string]any{"url": "https://evil.example/p"})
	clean := transcriptWith(t, "Read", map[string]any{"file_path": "/x"})
	publish := map[string]any{"url": "/x.md", "body": "# X\n", "metadata": map[string]any{"tags": "a"}}

	// The default is warn: a warning, no prompt and no stamp.
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: web}); d.Decision != "warn" || d.UpdatedInput != nil || !strings.Contains(d.Reason, "WebFetch https://evil.example") {
		t.Fatalf("default tainted publish = %q (%s), updated %v; want warn naming the read, no stamp", d.Decision, d.Reason, d.UpdatedInput)
	}

	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "ask")
	d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: web})
	if d.Decision != "ask" || !strings.Contains(d.Reason, "WebFetch https://evil.example") {
		t.Fatalf("tainted publish = %q (%s), want ask naming the read", d.Decision, d.Reason)
	}
	meta, _ := d.UpdatedInput["metadata"].(map[string]any)
	if meta[protocol.TaintKey] != protocol.TaintValue || meta[protocol.TaintFromKey] != "WebFetch https://evil.example" || meta["tags"] != "a" || d.UpdatedInput["body"] != "# X\n" {
		t.Errorf("stamp = %+v", d.UpdatedInput)
	}
	if _, touched := publish["metadata"].(map[string]any)[protocol.TaintKey]; touched {
		t.Error("stamp mutated the caller's input")
	}

	for _, verb := range []string{"archive", "curate", "promote", "graph_publish", "append"} {
		d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_" + verb, ToolInput: map[string]any{"url": "/x.md", "body": "x"}, TranscriptPath: web})
		if d.Decision != "ask" || d.UpdatedInput != nil {
			t.Errorf("tainted %s = %q, updated %v; want ask, no rewrite", verb, d.Decision, d.UpdatedInput)
		}
	}
	if d := mustEval(t, &Input{ToolName: "mcp__knowledge__mark_publish", ToolInput: map[string]any{"url": "mark://team/k.md", "body": "# K\n", "metadata": map[string]any{"tags": "category:guide", "type": "Guide"}}, TranscriptPath: web}); d.Decision != "ask" {
		t.Errorf("tainted knowledge publish = %q (%s)", d.Decision, d.Reason)
	}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_fetch", ToolInput: map[string]any{"url": "/x.md"}, TranscriptPath: web}); d.Decision != "allow" {
		t.Errorf("tainted read = %q, want allow", d.Decision)
	}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: clean}); d.Decision != "allow" {
		t.Errorf("clean publish = %q (%s), want allow", d.Decision, d.Reason)
	}
	if d := mustEval(t, &Input{Tool: "mark_publish", Input: publish, Taint: []string{"WebFetch https://a.example"}}); d.Decision != "ask" || !strings.Contains(d.Reason, "https://a.example") {
		t.Errorf("adapter taint = %q (%s), want ask", d.Decision, d.Reason)
	}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: filepath.Join(t.TempDir(), "gone.jsonl")}); d.Decision != "ask" || !strings.Contains(d.Reason, "could not be read") {
		t.Errorf("unreadable transcript = %q (%s), want ask", d.Decision, d.Reason)
	}
	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "off")
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: web}); d.Decision != "allow" {
		t.Errorf("taint off = %q, want allow", d.Decision)
	}
	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "warn")
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: web}); d.Decision != "warn" || d.UpdatedInput != nil {
		t.Errorf("taint warn = %q, updated %v", d.Decision, d.UpdatedInput)
	}
}

// No scan where the verdict cannot show: a post hook drops an ask, and a
// block already outranks it.
func TestTaintSkipsWhenItCannotMatter(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "ask")
	gone := filepath.Join(t.TempDir(), "gone.jsonl") // a scan would ask: unreadable
	publish := map[string]any{"url": "/x.md", "body": "# X\n", "metadata": map[string]any{"tags": "a"}}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: gone, Post: true}); d.Decision != "allow" {
		t.Errorf("post phase at ask = %q (%s), want no scan", d.Decision, d.Reason)
	}
	secret := map[string]any{"url": "/x.md", "body": "-----BEGIN RSA PRIVATE KEY-----\n", "metadata": map[string]any{"tags": "a"}}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: secret, TranscriptPath: gone}); d.Decision != "block" || strings.Contains(d.Reason, "transcript") {
		t.Errorf("block = %q (%s), want the secret block alone", d.Decision, d.Reason)
	}
	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "warn")
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, Taint: []string{"WebSearch"}, Post: true}); d.Decision != "warn" {
		t.Errorf("post phase at warn = %q, want warn", d.Decision)
	}
}
