package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
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
	// A set value that is no level asks: a typo must not weaken the guard.
	for _, invalid := range []string{"ASK", "strict"} {
		t.Setenv("DEMARKUS_TAINT_STRICTNESS", invalid)
		if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: web}); d.Decision != "ask" {
			t.Errorf("taint strictness %q = %q, want ask", invalid, d.Decision)
		}
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
	// A post hook cannot ask: an unreadable transcript warns there.
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: gone, Post: true}); d.Decision != "warn" || !strings.Contains(d.Reason, "could not be read") {
		t.Errorf("post phase, unreadable transcript = %q (%s), want warn", d.Decision, d.Reason)
	}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: gone}); d.Decision != "ask" {
		t.Errorf("pre phase, unreadable transcript = %q (%s), want ask", d.Decision, d.Reason)
	}
}

// A warn that includes the taint warning keeps its sources and the other
// warnings apart, so the two can be shown on different terms.
func TestTaintWarnKeepsOtherWarningsApart(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	web := transcriptWith(t, "WebFetch", map[string]any{"url": "https://evil.example/p"})
	eval := func(input map[string]any) Decision {
		return mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: input, TranscriptPath: web, Post: true})
	}
	tagged := eval(map[string]any{"url": "/x.md", "body": "# X\n", "metadata": map[string]any{"tags": "a"}})
	if tagged.Decision != "warn" || len(tagged.sources) != 1 || tagged.rest != "" {
		t.Errorf("tainted write = %q, sources %v, rest %q; want a warn with its source and no other warning", tagged.Decision, tagged.sources, tagged.rest)
	}
	untagged := eval(map[string]any{"url": "/x.md", "body": "# X\n"})
	if untagged.Decision != "warn" || len(untagged.sources) != 1 || !strings.Contains(untagged.rest, "metadata.tags") || strings.Contains(untagged.rest, "untrusted") {
		t.Errorf("untagged tainted write = %q, sources %v, rest %q; want the tag warning apart", untagged.Decision, untagged.sources, untagged.rest)
	}
	if !strings.Contains(untagged.Reason, "untrusted") || !strings.Contains(untagged.Reason, "metadata.tags") {
		t.Errorf("reason %q, want both warnings", untagged.Reason)
	}
}

// quiet warns the agent like warn; the user is shown the other warnings alone,
// and the session's one showing is not spent.
func TestTaintQuiet(t *testing.T) {
	home := setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "quiet")
	web := transcriptWith(t, "WebFetch", map[string]any{"url": "https://evil.example/p"})
	for _, post := range []bool{false, true} {
		in := &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: map[string]any{"url": "/x.md", "body": "# X\n"}, TranscriptPath: web, Post: post, SessionID: "session-a"}
		d := mustEval(t, in)
		if d.Decision != "warn" || d.UpdatedInput != nil || !strings.Contains(d.Reason, "untrusted") {
			t.Fatalf("post %v: quiet = %q (%s), updated %v; want an unstamped warn", post, d.Decision, d.Reason, d.UpdatedInput)
		}
		if notice, ok := Report(d, in); !ok || !strings.Contains(notice, "metadata.tags") || strings.Contains(notice, "untrusted") {
			t.Errorf("post %v: notice = %q, %v; want the tag warning alone", post, notice, ok)
		}
	}
	if got := claims(t, home, shownDir); len(got) != 0 {
		t.Errorf("shown = %v, want no showing spent", got)
	}
	// An unreadable transcript still asks before the call; after it, quietly warns.
	gone := filepath.Join(t.TempDir(), "gone.jsonl")
	publish := map[string]any{"url": "/x.md", "body": "# X\n", "metadata": map[string]any{"tags": "a"}}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: gone}); d.Decision != "ask" {
		t.Errorf("pre phase, unreadable transcript = %q, want ask", d.Decision)
	}
	if d := mustEval(t, &Input{ToolName: "mcp__demarkus-memory__mark_publish", ToolInput: publish, TranscriptPath: gone, Post: true}); d.Decision != "warn" || shown(d, "session-a") != "" {
		t.Errorf("post phase, unreadable transcript = %q, shown %q; want a quiet warn", d.Decision, shown(d, "session-a"))
	}
}

// A brand sets the level its users start from: under the env override and
// the ~/.demarkus file, over the built-in warn.
func TestTaintBrandDefault(t *testing.T) {
	home := setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	brand := filepath.Join(t.TempDir(), "brand.json")
	t.Setenv(config.BrandFileEnv, brand)
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in := func() *Input {
		return &Input{Tool: "mark_publish", Input: map[string]any{"url": "/x.md", "body": "# X\n", "metadata": map[string]any{"tags": "a"}}, Taint: []string{"WebSearch"}}
	}
	// A base plugin ships no brand file, and a brand need not set the level.
	if d := mustEval(t, in()); d.Decision != "warn" {
		t.Errorf("no brand file = %q, want warn", d.Decision)
	}
	write(brand, `{"plugin_name":"acme-brain","store_noun":"soul"}`)
	if d := mustEval(t, in()); d.Decision != "warn" {
		t.Errorf("brand without a level = %q, want warn", d.Decision)
	}
	for level, want := range map[string]string{"off": "allow", "quiet": "warn", "warn": "warn", "ask": "ask", "strict": "ask"} {
		write(brand, `{"plugin_name":"acme-brain","taint_strictness":"`+level+`"}`)
		d := mustEval(t, in())
		if d.Decision != want || d.quiet != (level == "quiet") {
			t.Errorf("brand %s = %q, quiet %v; want %s", level, d.Decision, d.quiet, want)
		}
	}
	write(brand, `{"taint_strictness":"off"}`)
	write(filepath.Join(home, ".demarkus", "plugin.taint-strictness"), "ask\n")
	if d := mustEval(t, in()); d.Decision != "ask" {
		t.Errorf("user file over brand = %q, want ask", d.Decision)
	}
	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "warn")
	if d := mustEval(t, in()); d.Decision != "warn" {
		t.Errorf("env over file and brand = %q, want warn", d.Decision)
	}
	// A brand file that does not parse is an error, never a silent default.
	t.Setenv("DEMARKUS_TAINT_STRICTNESS", "")
	if err := os.Remove(filepath.Join(home, ".demarkus", "plugin.taint-strictness")); err != nil {
		t.Fatal(err)
	}
	write(brand, `{"taint_strictness":`)
	if _, err := Evaluate(in()); err == nil || !strings.Contains(err.Error(), "brand.json") {
		t.Errorf("unparsable brand file: err = %v", err)
	}
}
