package nudge

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/facts"
)

// Signals are the session-end facts an adapter supplies or the transcript
// scan derives.
type Signals struct {
	ChangedFiles bool `json:"changedFiles"`
	// JournalWrite: a journal entry was written after the last project edit,
	// so the journal covers the session's work so far.
	JournalWrite bool `json:"journalWrite"`
	// MemoryWrite: any memory write, from adapters older than journalWrite,
	// which share ~/.demarkus/bin; remove once every plugin pins a binary with it.
	MemoryWrite bool `json:"memoryWrite"`
	// PendingBackground: a background agent or command has not reported back,
	// so this Stop is a pause mid-task, not the session's end.
	PendingBackground bool `json:"pendingBackground"`
}

// Scan is what a transcript yields: the signals plus the last outcome of each
// recognized check (main transcript only; subagent order is unknown).
type Scan struct {
	Signals
	Checks map[facts.Check]facts.Outcome
	// LastEdit: the last project file edited in the main transcript; its repo
	// is where the work is, even when the session started elsewhere.
	LastEdit string
}

var (
	notifiedIDRe        = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	taskNotificationTag = []byte("<task-notification>")
	toolBlockMarkers    = [][]byte{[]byte(`"tool_use"`), []byte(`"tool_result"`)}
)

// transcriptLine is the slice of a Claude Code JSONL record the scan reads.
// toolUseResult is harness metadata beside the message; isAsync marks an Agent
// launch, backgroundTaskId a backgrounded Bash command.
type transcriptLine struct {
	Type       string `json:"type"`
	Content    string `json:"content"` // queue-operation: the queued prompt
	Attachment struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
	} `json:"attachment"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult struct {
		IsAsync          bool   `json:"isAsync"`
		BackgroundTaskID string `json:"backgroundTaskId"`
	} `json:"toolUseResult"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	IsError   bool            `json:"is_error"`    // tool_result
	ExitCode  exitCodeContent `json:"content"`     // tool_result: content starts with "Exit code"
	Input     struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Command      string `json:"command"`
		URL          string `json:"url"`
		Route        string `json:"route"`
	} `json:"input"`
}

// ScanTranscript derives the session-end signals from a Claude Code JSONL
// transcript plus its subagents' (<dir>/<session>/subagents/*.jsonl): edits and
// journal writes only, and a subagent's current journal counts (order unknown).
func ScanTranscript(path string) (Scan, error) {
	sig, err := scanFile(path)
	if err != nil {
		return Scan{}, err
	}
	subs, err := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	if err != nil {
		return Scan{}, err
	}
	for _, sub := range subs {
		ss, err := scanFile(sub)
		if os.IsNotExist(err) {
			continue // rotated away between Glob and Open; supplementary, not fatal
		}
		if err != nil {
			return Scan{}, err
		}
		sig.ChangedFiles = sig.ChangedFiles || ss.ChangedFiles
		sig.JournalWrite = sig.JournalWrite || ss.JournalWrite
	}
	return sig, nil
}

func scanFile(path string) (Scan, error) {
	f, err := os.Open(path)
	if err != nil {
		return Scan{}, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return scanTranscript(f)
}

type scanner struct {
	sig     Scan
	pending map[string]bool          // background tool_use ids without a task-notification yet
	checks  map[string]facts.Command // Bash tool_use ids running a recognized check, awaiting a result
}

func scanTranscript(r io.Reader) (Scan, error) {
	s := scanner{
		sig:     Scan{Checks: map[facts.Check]facts.Outcome{}},
		pending: map[string]bool{},
		checks:  map[string]facts.Command{},
	}
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

// line reads tool_use blocks and harness metadata only, so tool output, pasted
// text, and the system-prompt snapshot cannot fake a signal.
func (s *scanner) line(line []byte) {
	hasNotification := bytes.Contains(line, taskNotificationTag)
	if !hasNotification && !containsAny(line, toolBlockMarkers) {
		return // fast reject; the parse below stays authoritative
	}
	var tl transcriptLine
	if err := json.Unmarshal(line, &tl); err != nil {
		return // partial or foreign line: nothing to learn from it
	}
	if hasNotification {
		for _, m := range notifiedIDRe.FindAllStringSubmatch(notificationText(&tl), -1) {
			delete(s.pending, m[1])
		}
	}
	if len(tl.Message.Content) == 0 || tl.Message.Content[0] != '[' {
		return // string content carries no tool blocks
	}
	var blocks []contentBlock
	if err := json.Unmarshal(tl.Message.Content, &blocks); err != nil {
		return
	}
	background := tl.ToolUseResult.IsAsync || tl.ToolUseResult.BackgroundTaskID != ""
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case "tool_use":
			switch b.Name {
			case "Edit", "MultiEdit", "Write", "NotebookEdit":
				if !isScratchPath(b.Input.FilePath) && !isScratchPath(b.Input.NotebookPath) {
					s.sig.ChangedFiles = true
					s.sig.JournalWrite = false // an edit after the entry leaves it stale
					s.sig.LastEdit = cmp.Or(b.Input.FilePath, b.Input.NotebookPath)
				}
			case "Bash":
				if cmd := facts.Classify(b.Input.Command); len(cmd.Checks) > 0 {
					s.checks[b.ID] = cmd
				}
			}
			if config.IsJournalWrite(b.Name, b.Input.URL, b.Input.Route) {
				s.sig.JournalWrite = true
			}
		case "tool_result":
			if background {
				s.pending[b.ToolUseID] = true
			}
			s.checkResult(b, background)
		}
	}
}

// exitCodeContent decodes a tool_result's content only to whether it starts
// with the harness's "Exit code" line, so result bodies are never copied.
type exitCodeContent bool

func (e *exitCodeContent) UnmarshalJSON(data []byte) error {
	*e = exitCodeContent(bytes.HasPrefix(data, []byte(`"Exit code `)))
	return nil
}

// checkResult records a recognized check's outcome. A failed exit is is_error
// plus the "Exit code" prefix; a rejected or timed-out call sets is_error
// without one. Background runs report later, unread here.
func (s *scanner) checkResult(b *contentBlock, background bool) {
	cmd, ok := s.checks[b.ToolUseID]
	if !ok {
		return
	}
	delete(s.checks, b.ToolUseID)
	if background {
		return
	}
	if b.IsError && !bool(b.ExitCode) {
		return
	}
	outcome := cmd.Outcome(!b.IsError)
	for _, c := range cmd.Checks {
		s.sig.Checks[c] = outcome
	}
}

// notificationText: the harness-written prompt carrying a task-notification
// (queue entry, queued-command attachment, or string user turn). Tool results
// are arrays, so a notification quoted inside tool output never joins.
func notificationText(tl *transcriptLine) string {
	switch tl.Type {
	case "queue-operation":
		return tl.Content
	case "attachment":
		if tl.Attachment.Type == "queued_command" {
			return tl.Attachment.Prompt
		}
	case "user":
		var text string
		if len(tl.Message.Content) > 0 && tl.Message.Content[0] == '"' && json.Unmarshal(tl.Message.Content, &text) == nil {
			return text
		}
	}
	return ""
}

// isScratchPath: temp files and the harness scratchpad are not project work
// worth a journal entry. An unknown (empty) path counts as project work.
func isScratchPath(p string) bool {
	if p == "" {
		return false
	}
	p = filepath.Clean(p)
	for _, prefix := range []string{os.TempDir(), "/tmp", "/private/tmp"} {
		if strings.HasPrefix(p, filepath.Clean(prefix)+string(filepath.Separator)) {
			return true
		}
	}
	return strings.Contains(p, string(filepath.Separator)+"scratchpad"+string(filepath.Separator))
}

func containsAny(line []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(line, n) {
			return true
		}
	}
	return false
}
