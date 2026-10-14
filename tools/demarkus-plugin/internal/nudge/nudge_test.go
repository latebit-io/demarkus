package nudge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func eval(t *testing.T, in *Input) string {
	t.Helper()
	o, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return o.Nudge
}

func TestRecallMemory(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	if eval(t, &Input{Event: "recall", Surface: "memory", Prompt: "did we decide on the cache format?"}) == "" {
		t.Error("recall question should nudge")
	}
	if eval(t, &Input{Event: "recall", Surface: "memory", Prompt: "write a function to add two numbers"}) != "" {
		t.Error("plain prompt should not nudge")
	}
}

func TestRecallMemorySilentWithoutMemory(t *testing.T) {
	setupHome(t, nil) // no plugin-memory.conf
	if eval(t, &Input{Event: "recall", Surface: "memory", Prompt: "did we decide?"}) != "" {
		t.Error("no memory configured → no nudge")
	}
}

func TestRecallKnowledge(t *testing.T) {
	setupHome(t, map[string]string{"knowledge-systems": "knowledge\n"})
	if eval(t, &Input{Event: "recall", Surface: "knowledge", Prompt: "what does the org standard say?"}) == "" {
		t.Error("org recall should nudge when a system is joined")
	}
	setupHome(t, nil)
	if eval(t, &Input{Event: "recall", Surface: "knowledge", Prompt: "what does the org say?"}) != "" {
		t.Error("no joined system → no knowledge nudge")
	}
}

func TestPromote(t *testing.T) {
	setupHome(t, map[string]string{
		"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n",
		"knowledge-systems":  "knowledge\n", // a promote destination exists
	})
	base := func(url, body string) *Input {
		return &Input{Event: "promote", Tool: "demarkus_memory_mark_publish", Input: map[string]any{"url": url, "body": body}}
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
	if eval(t, &Input{Event: "promote", Tool: "demarkus_memory_mark_publish", Input: map[string]any{"url": "/p/adr/0001.md", "body": "d"}}) != "" {
		t.Error("no promote destination → no nudge")
	}
}

func TestSessionEnd(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	sig := func(changed, write, pending bool) *Input {
		return &Input{Event: "session-end", Signals: Signals{ChangedFiles: changed, MemoryWrite: write, PendingBackground: pending}}
	}
	if eval(t, sig(true, false, false)) == "" {
		t.Error("changed files + no memory write → journal nudge")
	}
	if eval(t, sig(true, true, false)) != "" {
		t.Error("a legacy memoryWrite from an older adapter still suppresses the nudge")
	}
	if eval(t, &Input{Event: "session-end", Signals: Signals{ChangedFiles: true, JournalWrite: true}}) != "" {
		t.Error("a journal write suppresses the nudge")
	}
	if eval(t, sig(false, false, false)) != "" {
		t.Error("no file changes → no nudge")
	}
	if eval(t, sig(true, false, true)) != "" {
		t.Error("a pending background agent means the session is paused, not over")
	}
	in := sig(true, false, false)
	in.StopHookActive = true
	if eval(t, in) != "" {
		t.Error("stop_hook_active=true must not block again")
	}
}

func TestSessionEndScansTranscript(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(editLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if eval(t, &Input{Event: "session-end", TranscriptPath: path}) == "" {
		t.Error("an Edit in the transcript should nudge")
	}
	if eval(t, &Input{Event: "session-end", TranscriptPath: path, Signals: Signals{JournalWrite: true}}) != "" {
		t.Error("adapter-supplied journalWrite OR-merges with the scan")
	}
	planOnly := filepath.Join(t.TempDir(), "p.jsonl")
	planLine := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"p","name":"mcp__soul__mark_publish","input":{"url":"/plans/x.md"}}]}}`
	if err := os.WriteFile(planOnly, []byte(editLine+"\n"+planLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if eval(t, &Input{Event: "session-end", TranscriptPath: planOnly}) == "" {
		t.Error("a plan edit is not a journal entry; the nudge still fires")
	}
	if _, err := Evaluate(&Input{Event: "session-end", TranscriptPath: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("unreadable transcript must surface an error, not nudge silently")
	}
}

func TestPromoteAcceptsCursorShape(t *testing.T) {
	setupHome(t, map[string]string{
		"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n",
		"knowledge-systems":  "knowledge\n",
	})
	got := eval(t, &Input{Event: "promote", ToolName: "mark_publish", McpServerName: "demarkus-memory",
		ToolInput: map[string]any{"url": "/proj/adr/0001-x.md", "body": "# x"}})
	if got == "" {
		t.Fatal("cursor-shaped ADR publish should nudge")
	}
	got = eval(t, &Input{Event: "promote", ToolName: "mark_publish", McpServerName: "knowledge",
		ToolInput: map[string]any{"url": "/proj/adr/0001-x.md", "body": "# x"}})
	if got != "" {
		t.Fatal("publish to the knowledge system must not nudge promotion")
	}
}

func TestSessionEndCarriesFacts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	body := strings.Join([]string{editLine, bashUse("t1", "go test ./..."), bashResult("t1", true, `Exit code 1`)}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := eval(t, &Input{Event: "session-end", TranscriptPath: path, Cwd: repo})
	for _, want := range []string{journalNudgeHead, "- branch: main (no commits yet)", "- uncommitted (1): new.go", "- checks: test failed", journalNudgeFactsUse, journalNudgeAsk} {
		if !strings.Contains(got, want) {
			t.Errorf("nudge missing %q:\n%s", want, got)
		}
	}

	got = eval(t, &Input{Event: "session-end", TranscriptPath: path, Cwd: t.TempDir()})
	if strings.Contains(got, "branch:") || !strings.Contains(got, "- checks: test failed") {
		t.Errorf("outside a repo the nudge keeps the checks and drops git facts:\n%s", got)
	}
}

func TestSessionEndReadsTheEditedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	// Outside the temp dir: the scanner treats temp paths as scratch, not project work.
	repo, err := os.MkdirTemp(".", "_testrepo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(repo); err != nil {
			t.Errorf("remove %s: %v", repo, err)
		}
	})
	if repo, err = filepath.Abs(repo); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "feat/elsewhere").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	edit := func(path string) string {
		dir := t.TempDir()
		p := filepath.Join(dir, "t.jsonl")
		line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e","name":"Edit","input":{"file_path":"` + path + `"}}]}}`
		if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Session started outside any repo; the edit was in repo/pkg.
	got := eval(t, &Input{Event: "session-end", TranscriptPath: edit(filepath.Join(repo, "pkg", "a.go")), Cwd: t.TempDir()})
	if !strings.Contains(got, "- branch: feat/elsewhere (no commits yet) in "+filepath.Base(repo)) {
		t.Errorf("git should be read where the last edit happened:\n%s", got)
	}

	// Edit outside any repo: fall back to the session cwd.
	got = eval(t, &Input{Event: "session-end", TranscriptPath: edit("/nonexistent/dir/a.go"), Cwd: repo})
	if !strings.Contains(got, "- branch: feat/elsewhere") {
		t.Errorf("an edit outside any repo falls back to cwd:\n%s", got)
	}
}
