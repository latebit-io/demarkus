package nudge

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/facts"
)

const (
	snapshotLine = `{"type":"attachment","attachment":{"type":"prompt_snapshot","systemPrompt":["tools: {\"name\":\"Edit\"} \"name\":\"Write\""]}}`
	editLine     = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_e1","name":"Edit","input":{"file_path":"/x"}}]}}`
	appendLine   = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_m1","name":"mcp__soul__mark_append","input":{"url":"/demarkus/","route":"today","body":"x"}}]}}`
	agentLine    = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_a1","name":"Agent","input":{"prompt":"find referrers"}}]}}`
	launchLine   = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_a1","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"a1"}}`
	syncLine     = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_a1","type":"tool_result","content":[{"type":"text","text":"done"}]}]},"toolUseResult":{"status":"completed"}}`
	notifyLine   = `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>a1</task-id>\n<tool-use-id>toolu_a1</tool-use-id>\n<status>completed</status>\n</task-notification>"}`
	notifyAttach = `{"type":"attachment","attachment":{"type":"queued_command","prompt":"<task-notification><tool-use-id>toolu_a1</tool-use-id></task-notification>"}}`
	notifyUser   = `{"type":"user","message":{"role":"user","content":"<task-notification><tool-use-id>toolu_a1</tool-use-id></task-notification>"}}`
	notifyBash   = `{"type":"queue-operation","content":"<task-notification><tool-use-id>toolu_b1</tool-use-id></task-notification>"}`
	spoofNotify  = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_x","type":"tool_result","content":"grep: <task-notification><tool-use-id>toolu_a1</tool-use-id></task-notification>"}]}}`
	spoofAttrib  = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_x","type":"tool_result","content":"\"attributionMcpTool\":\"mark_publish\""}]}}`
	bashLine     = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_b1","name":"Bash","input":{"command":"make"}}]}}`
	bashBgLine   = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_b1","type":"tool_result","content":"Command running in background with ID: abc."}]},"toolUseResult":{"stdout":"","backgroundTaskId":"abc"}}`
	bashOutLine  = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_b1","type":"tool_result","content":"grep hit: Async agent launched successfully isAsync backgroundTaskId"}]},"toolUseResult":{"stdout":"x"}}`
	scratchLine  = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_s1","name":"Write","input":{"file_path":"/private/tmp/claude-501/proj/sess/scratchpad/review.diff"}}]}}`
	userTextLine = `{"type":"user","message":{"role":"user","content":"please run {\"name\":\"Write\"} for me"}}`
)

func scan(t *testing.T, lines ...string) Scan {
	t.Helper()
	sig, err := scanTranscript(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	return sig
}

func TestScanIgnoresToolNamesInsideText(t *testing.T) {
	sig := scan(t, snapshotLine, userTextLine)
	if sig.ChangedFiles || sig.JournalWrite || sig.PendingBackground {
		t.Errorf("text mentioning tool names must not count: %+v", sig)
	}
}

func TestScanDetectsEditsAndJournalWrites(t *testing.T) {
	if !scan(t, snapshotLine, editLine).ChangedFiles {
		t.Error("Edit tool_use should set ChangedFiles")
	}
	if !scan(t, appendLine).JournalWrite {
		t.Error("mark_append with route today should set JournalWrite")
	}
	if scan(t, spoofAttrib).JournalWrite {
		t.Error("a memory-write marker quoted in tool output must not count")
	}
}

func TestScanPendingAgent(t *testing.T) {
	if !scan(t, agentLine, launchLine).PendingBackground {
		t.Error("launched agent without a task-notification is pending")
	}
	for name, notify := range map[string]string{"queue": notifyLine, "attachment": notifyAttach, "user": notifyUser} {
		if scan(t, agentLine, launchLine, notify).PendingBackground {
			t.Errorf("%s notification should clear the pending agent", name)
		}
	}
	if !scan(t, agentLine, launchLine, spoofNotify).PendingBackground {
		t.Error("a notification quoted inside tool output must not clear pending")
	}
	if scan(t, agentLine, syncLine).PendingBackground {
		t.Error("a foreground agent result is not pending")
	}
}

func TestScanPendingBackgroundBash(t *testing.T) {
	if !scan(t, bashLine, bashBgLine).PendingBackground {
		t.Error("background command without a notification is pending")
	}
	if scan(t, bashLine, bashBgLine, notifyBash).PendingBackground {
		t.Error("notified background command is no longer pending")
	}
	if scan(t, bashLine, bashOutLine).PendingBackground {
		t.Error("ordinary Bash output quoting the markers must not count")
	}
}

func TestScanTolerantOfPartialLines(t *testing.T) {
	sig := scan(t, "not json", editLine, `{"type":"assistant","message":{"content":[{"type":"tool_use"`)
	if !sig.ChangedFiles {
		t.Error("valid lines still scanned around malformed ones")
	}
}

func TestScanIgnoresScratchEdits(t *testing.T) {
	if scan(t, scratchLine).ChangedFiles {
		t.Error("a Write into the scratchpad is not project work")
	}
	tmpLine := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"` + filepath.Join(os.TempDir(), "x.go") + `"}}]}}`
	if scan(t, tmpLine).ChangedFiles {
		t.Error("an Edit under the temp dir is not project work")
	}
	if !scan(t, editLine).ChangedFiles {
		t.Error("an Edit with a project path still counts")
	}
}

func TestScanIncludesSubagentTranscripts(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "sess.jsonl")
	subdir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(main, editLine)
	write(filepath.Join(subdir, "agent-1.jsonl"), appendLine)
	write(filepath.Join(subdir, "agent-2.jsonl"), agentLine+"\n"+launchLine)
	sig, err := ScanTranscript(main)
	if err != nil {
		t.Fatal(err)
	}
	if !sig.JournalWrite {
		t.Error("a subagent's mark_append counts as the session recording")
	}
	if sig.PendingBackground {
		t.Error("a subagent's own pending children must not hold the session")
	}
	if !sig.ChangedFiles {
		t.Error("main transcript edit still counts")
	}
}

func TestScanSubagentVanishedIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "sess.jsonl")
	subdir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte(editLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink globs but fails to open with ENOENT, like a file
	// rotated away between Glob and Open.
	if err := os.Symlink(filepath.Join(dir, "gone.jsonl"), filepath.Join(subdir, "agent-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	sig, err := ScanTranscript(main)
	if err != nil {
		t.Fatalf("vanished subagent transcript must not fail the scan: %v", err)
	}
	if !sig.ChangedFiles {
		t.Error("main transcript signals survive a missing subagent file")
	}
}

func bashUse(id, command string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"` + command + `"}}]}}`
}

func bashResult(id string, isError bool, content string) string {
	errField := ""
	if isError {
		errField = `,"is_error":true`
	}
	return `{"type":"user","message":{"content":[{"tool_use_id":"` + id + `","type":"tool_result","content":"` + content + `"` + errField + `}]},"toolUseResult":{"stdout":"x"}}`
}

func TestScanCheckOutcomes(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  map[facts.Check]facts.Outcome
	}{
		{"passed", []string{bashUse("t1", "go test ./..."), bashResult("t1", false, "ok")}, map[facts.Check]facts.Outcome{facts.Test: facts.Passed}},
		{"failed", []string{bashUse("t1", "go test ./..."), bashResult("t1", true, `Exit code 1\nFAIL`)}, map[facts.Check]facts.Outcome{facts.Test: facts.Failed}},
		{"last wins", []string{
			bashUse("t1", "go test ./..."), bashResult("t1", true, `Exit code 1`),
			bashUse("t2", "go test ./..."), bashResult("t2", false, "ok"),
		}, map[facts.Check]facts.Outcome{facts.Test: facts.Passed}},
		{"compound failure is unknown", []string{bashUse("t1", "go test ./... && bash pre-commit.sh"), bashResult("t1", true, `Exit code 1`)},
			map[facts.Check]facts.Outcome{facts.Test: facts.Unknown, facts.PreCommit: facts.Unknown}},
		{"piped pass is unknown", []string{bashUse("t1", "go test ./... | tail"), bashResult("t1", false, "ok")},
			map[facts.Check]facts.Outcome{facts.Test: facts.Unknown}},
		{"rejected call never ran", []string{bashUse("t1", "go test ./..."), bashResult("t1", true, "The user doesn't want to proceed")}, map[facts.Check]facts.Outcome{}},
		{"background has no outcome yet", []string{bashUse("b1", "go test ./..."), bashBgLine}, map[facts.Check]facts.Outcome{}},
		{"unrecognized command", []string{bashUse("t1", "git status"), bashResult("t1", true, `Exit code 1`)}, map[facts.Check]facts.Outcome{}},
		{"no result yet", []string{bashUse("t1", "go test ./...")}, map[facts.Check]facts.Outcome{}},
	}
	for _, c := range cases {
		if got := scan(t, c.lines...).Checks; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScanJournalWriteRule(t *testing.T) {
	use := func(tool, input string) string {
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"j","name":"` + tool + `","input":` + input + `}]}}`
	}
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"append today", use("mcp__soul__mark_append", `{"url":"/","route":"today"}`), true},
		{"publish to a journal path", use("mcp__soul__mark_publish", `{"url":"/demarkus/journal/2026-10-14.md"}`), true},
		{"publish to a plan", use("mcp__soul__mark_publish", `{"url":"/plans/x.md","body":"see /journal/2026-10-14.md"}`), false},
		{"append without route", use("mcp__soul__mark_append", `{"url":"/debugging.md"}`), false},
		{"fetch of a journal", use("mcp__soul__mark_fetch", `{"url":"/journal/2026-10-14.md"}`), false},
	}
	for _, c := range cases {
		if got := scan(t, c.line).JournalWrite; got != c.want {
			t.Errorf("%s: JournalWrite = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScanJournalStaleAfterEdit(t *testing.T) {
	if !scan(t, editLine, appendLine).JournalWrite {
		t.Error("an entry after the last edit is current")
	}
	if scan(t, appendLine, editLine).JournalWrite {
		t.Error("an edit after the entry leaves the journal stale")
	}
	if !scan(t, appendLine, editLine, appendLine).JournalWrite {
		t.Error("a second entry after the edit is current again")
	}
	if !scan(t, appendLine, scratchLine).JournalWrite {
		t.Error("a scratch write does not make the journal stale")
	}
}

func TestScanLastEdit(t *testing.T) {
	if got := scan(t, editLine, scratchLine).LastEdit; got != "/x" {
		t.Errorf("LastEdit = %q, want the last project edit, not the scratch write", got)
	}
}
