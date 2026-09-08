package nudge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	snapshotLine = `{"type":"attachment","attachment":{"type":"prompt_snapshot","systemPrompt":["tools: {\"name\":\"Edit\"} \"name\":\"Write\""]}}`
	editLine     = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_e1","name":"Edit","input":{"file_path":"/x"}}]}}`
	appendLine   = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_m1","name":"mcp__soul__mark_append","input":{}}]}}`
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

func scan(t *testing.T, lines ...string) Signals {
	t.Helper()
	sig, err := scanTranscript(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	return sig
}

func TestScanIgnoresToolNamesInsideText(t *testing.T) {
	sig := scan(t, snapshotLine, userTextLine)
	if sig.ChangedFiles || sig.MemoryWrite || sig.PendingBackground {
		t.Errorf("text mentioning tool names must not count: %+v", sig)
	}
}

func TestScanDetectsEditsAndMemoryWrites(t *testing.T) {
	if !scan(t, snapshotLine, editLine).ChangedFiles {
		t.Error("Edit tool_use should set ChangedFiles")
	}
	if !scan(t, appendLine).MemoryWrite {
		t.Error("mark_append tool_use should set MemoryWrite")
	}
	if scan(t, spoofAttrib).MemoryWrite {
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
	if !sig.MemoryWrite {
		t.Error("a subagent's mark_append counts as the session recording")
	}
	if sig.PendingBackground {
		t.Error("a subagent's own pending children must not hold the session")
	}
	if !sig.ChangedFiles {
		t.Error("main transcript edit still counts")
	}
}
