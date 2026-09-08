package nudge

import (
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
	bashLine     = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_b1","name":"Bash","input":{"command":"make"}}]}}`
	bashBgLine   = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_b1","type":"tool_result","content":"Command running in background with ID: abc."}]},"toolUseResult":{"stdout":"","backgroundTaskId":"abc"}}`
	bashOutLine  = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_b1","type":"tool_result","content":"grep hit: Async agent launched successfully isAsync backgroundTaskId"}]},"toolUseResult":{"stdout":"x"}}`
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
	if !scan(t, `{"type":"file-history-snapshot","attributionMcpTool":"mark_publish"}`).MemoryWrite {
		t.Error("attributionMcpTool marker should set MemoryWrite")
	}
}

func TestScanPendingAgent(t *testing.T) {
	if !scan(t, agentLine, launchLine).PendingBackground {
		t.Error("launched agent without a task-notification is pending")
	}
	if scan(t, agentLine, launchLine, notifyLine).PendingBackground {
		t.Error("notified agent is no longer pending")
	}
	if scan(t, agentLine, syncLine).PendingBackground {
		t.Error("a foreground agent result is not pending")
	}
}

func TestScanPendingBackgroundBash(t *testing.T) {
	if !scan(t, bashLine, bashBgLine).PendingBackground {
		t.Error("background command without a notification is pending")
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
