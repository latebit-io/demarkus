package nudge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Signals are the session-end facts an adapter supplies or the transcript
// scan derives.
type Signals struct {
	ChangedFiles bool `json:"changedFiles"`
	MemoryWrite  bool `json:"memoryWrite"`
	// PendingBackground: a background agent or command has not reported back,
	// so this Stop is a pause mid-task, not the session's end.
	PendingBackground bool `json:"pendingBackground"`
}

var (
	notifiedIDRe        = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	taskNotificationTag = []byte("<task-notification>")
	attributionMarkers  = [][]byte{[]byte(`"attributionMcpTool":"mark_publish"`), []byte(`"attributionMcpTool":"mark_append"`)}
	toolBlockMarkers    = [][]byte{[]byte(`"tool_use"`), []byte(`"tool_result"`)}
)

// transcriptLine is the slice of a Claude Code JSONL record the scan reads.
// toolUseResult is harness metadata beside the message; isAsync marks an Agent
// launch, backgroundTaskId a backgrounded Bash command.
type transcriptLine struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult struct {
		IsAsync          bool   `json:"isAsync"`
		BackgroundTaskID string `json:"backgroundTaskId"`
	} `json:"toolUseResult"`
}

type contentBlock struct {
	Type      string `json:"type"`
	Name      string `json:"name"`        // tool_use
	ToolUseID string `json:"tool_use_id"` // tool_result
}

// ScanTranscript derives the session-end signals from a Claude Code JSONL
// transcript. Facts come from tool_use blocks and harness metadata only, so
// tool output, pasted text, and the system-prompt snapshot cannot fake them.
func ScanTranscript(path string) (Signals, error) {
	f, err := os.Open(path)
	if err != nil {
		return Signals{}, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return scanTranscript(f)
}

type scanner struct {
	sig     Signals
	pending map[string]bool // background tool_use ids without a task-notification yet
}

func scanTranscript(r io.Reader) (Signals, error) {
	s := scanner{pending: map[string]bool{}}
	// ReadBytes, not bufio.Scanner: tool results make single lines well past
	// any fixed token cap.
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			s.line(line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return s.sig, err
		}
	}
	s.sig.PendingBackground = len(s.pending) > 0
	return s.sig, nil
}

func (s *scanner) line(line []byte) {
	if !s.sig.MemoryWrite && containsAny(line, attributionMarkers) {
		s.sig.MemoryWrite = true
	}
	// Completion notices arrive as queued commands, not content blocks; the
	// tool-use id is the only join key to the launch.
	if bytes.Contains(line, taskNotificationTag) {
		for _, m := range notifiedIDRe.FindAllSubmatch(line, -1) {
			delete(s.pending, string(m[1]))
		}
	}
	if !containsAny(line, toolBlockMarkers) {
		return // fast reject; the parse below stays authoritative
	}
	var tl transcriptLine
	if err := json.Unmarshal(line, &tl); err != nil {
		return // partial or foreign line: nothing to learn from it
	}
	if len(tl.Message.Content) == 0 || tl.Message.Content[0] != '[' {
		return // string content carries no tool blocks
	}
	var blocks []contentBlock
	if err := json.Unmarshal(tl.Message.Content, &blocks); err != nil {
		return
	}
	background := tl.ToolUseResult.IsAsync || tl.ToolUseResult.BackgroundTaskID != ""
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			switch b.Name {
			case "Edit", "MultiEdit", "Write", "NotebookEdit":
				s.sig.ChangedFiles = true
			}
			if pt, ok := config.ParseTool(b.Name); ok && (pt.Verb == "publish" || pt.Verb == "append") {
				s.sig.MemoryWrite = true
			}
		case "tool_result":
			if background {
				s.pending[b.ToolUseID] = true
			}
		}
	}
}

func containsAny(line []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(line, n) {
			return true
		}
	}
	return false
}
