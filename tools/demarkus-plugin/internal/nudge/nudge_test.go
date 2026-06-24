package nudge

import (
	"os"
	"path/filepath"
	"testing"
)

func setupHome(t *testing.T, files map[string]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dm := filepath.Join(home, ".demarkus")
	if err := os.MkdirAll(dm, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dm, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func eval(t *testing.T, in Input) string {
	t.Helper()
	o, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return o.Nudge
}

func TestRecallMemory(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	if eval(t, Input{Event: "recall", Surface: "memory", Prompt: "did we decide on the cache format?"}) == "" {
		t.Error("recall question should nudge")
	}
	if eval(t, Input{Event: "recall", Surface: "memory", Prompt: "write a function to add two numbers"}) != "" {
		t.Error("plain prompt should not nudge")
	}
}

func TestRecallMemorySilentWithoutSoul(t *testing.T) {
	setupHome(t, nil) // no plugin-memory.conf
	if eval(t, Input{Event: "recall", Surface: "memory", Prompt: "did we decide?"}) != "" {
		t.Error("no soul configured → no nudge")
	}
}

func TestRecallKnowledge(t *testing.T) {
	setupHome(t, map[string]string{"knowledge-systems": "knowledge\n"})
	if eval(t, Input{Event: "recall", Surface: "knowledge", Prompt: "what does the org standard say?"}) == "" {
		t.Error("org recall should nudge when a system is joined")
	}
	setupHome(t, nil)
	if eval(t, Input{Event: "recall", Surface: "knowledge", Prompt: "what does the org say?"}) != "" {
		t.Error("no joined system → no knowledge nudge")
	}
}

func TestPromote(t *testing.T) {
	setupHome(t, map[string]string{
		"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n",
		"knowledge-systems":  "knowledge\n", // a promote destination exists
	})
	base := func(url, body string) Input {
		return Input{Event: "promote", Tool: "demarkus_memory_mark_publish", Input: map[string]any{"url": url, "body": body}}
	}
	if eval(t, base("/proj/adr/0001-x.md", "decision")) == "" {
		t.Error("fresh ADR with a destination should nudge")
	}
	if eval(t, base("/proj/notes.md", "x")) != "" {
		t.Error("non-ADR should not nudge")
	}
	if eval(t, base("/proj/adr/0001-x.md", "promoted: mark://k/x@v1")) != "" {
		t.Error("already-promoted ADR should not re-nudge")
	}
}

func TestPromoteNoDestination(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	if eval(t, Input{Event: "promote", Tool: "demarkus_memory_mark_publish", Input: map[string]any{"url": "/p/adr/0001.md", "body": "d"}}) != "" {
		t.Error("no promote destination → no nudge")
	}
}

func TestSessionEnd(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	if eval(t, Input{Event: "session-end", ChangedFiles: true, SoulWrite: false}) == "" {
		t.Error("changed files + no soul write → journal nudge")
	}
	if eval(t, Input{Event: "session-end", ChangedFiles: true, SoulWrite: true}) != "" {
		t.Error("a soul write suppresses the nudge")
	}
	if eval(t, Input{Event: "session-end", ChangedFiles: false, SoulWrite: false}) != "" {
		t.Error("no file changes → no nudge")
	}
}
