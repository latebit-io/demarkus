package answerbench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Usage follows OpenCode 1.18's disjoint input, output, reasoning, and cache buckets.
// Total is a cross-check, not an extra bucket.
type Usage struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
	Total *int64 `json:"total,omitempty"`
}

// Tokens includes cached input and reasoning exactly once.
func (u *Usage) Tokens() int64 {
	return u.Input + u.Output + u.Reasoning + u.Cache.Read + u.Cache.Write
}

// Add accumulates completed model turns without summing their redundant totals.
func (u *Usage) Add(v Usage) {
	u.Input += v.Input
	u.Output += v.Output
	u.Reasoning += v.Reasoning
	u.Cache.Read += v.Cache.Read
	u.Cache.Write += v.Cache.Write
}

// ToolCall retains the actual reader-visible input and result for citation checks.
type ToolCall struct {
	Name   string         `json:"name"`
	Input  map[string]any `json:"input"`
	Output string         `json:"output"`
	Error  string         `json:"error,omitempty"`
}

// Trace distinguishes known spend from complete token accounting.
type Trace struct {
	Session       string     `json:"session"`
	Usage         Usage      `json:"usage"`
	UsageComplete bool       `json:"usage_complete"`
	Turns         int        `json:"turns"`
	Calls         []ToolCall `json:"calls"`
	Final         string     `json:"final"`
	Errors        []string   `json:"errors,omitempty"`
}

type event struct {
	Type    string          `json:"type"`
	Session string          `json:"sessionID"`
	Error   json.RawMessage `json:"error"`
	Part    struct {
		ID      string `json:"id"`
		Message string `json:"messageID"`
		Text    string `json:"text"`
		Reason  string `json:"reason"`
		Tokens  *Usage `json:"tokens"`
		Tool    string `json:"tool"`
		State   struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input"`
			Output string         `json:"output"`
			Error  string         `json:"error"`
		} `json:"state"`
	} `json:"part"`
}

type eventLog struct {
	trace                  Trace
	seen, starts, finishes map[string]bool
	texts                  map[string][]string
	finalMessage           string
}

// ParseEvents preserves completed usage even when a later event is malformed.
func ParseEvents(r io.Reader) (Trace, error) {
	log := eventLog{seen: make(map[string]bool), starts: make(map[string]bool), finishes: make(map[string]bool), texts: make(map[string][]string)}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return log.trace, fmt.Errorf("decode OpenCode event: %w", err)
		}
		if err := log.consume(&e); err != nil {
			return log.trace, err
		}
	}
	if err := scanner.Err(); err != nil {
		return log.trace, fmt.Errorf("read OpenCode events: %w", err)
	}
	log.trace.Final = strings.Join(log.texts[log.finalMessage], "\n")
	log.trace.UsageComplete = len(log.trace.Errors) == 0 && len(log.starts) > 0 && len(log.starts) == len(log.finishes) && log.finalMessage != "" && log.trace.Final != ""
	for message := range log.starts {
		log.trace.UsageComplete = log.trace.UsageComplete && log.finishes[message]
	}
	return log.trace, nil
}

func (log *eventLog) consume(e *event) error {
	if log.trace.Session == "" {
		log.trace.Session = e.Session
	}
	if e.Session == "" || e.Session != log.trace.Session {
		return fmt.Errorf("mixed or missing session ID")
	}
	if e.Type == "error" {
		log.trace.Errors = append(log.trace.Errors, string(e.Error))
		return nil
	}
	if e.Part.ID == "" || e.Part.Message == "" {
		return fmt.Errorf("%s event missing part/message identity", e.Type)
	}
	key := e.Type + ":" + e.Part.ID
	if log.seen[key] {
		return nil
	}
	log.seen[key] = true
	switch e.Type {
	case "step_start":
		log.starts[e.Part.Message] = true
	case "step_finish":
		return log.finish(e)
	case "text":
		log.texts[e.Part.Message] = append(log.texts[e.Part.Message], e.Part.Text)
	case "tool_use":
		log.trace.Calls = append(log.trace.Calls, ToolCall{Name: e.Part.Tool, Input: e.Part.State.Input, Output: e.Part.State.Output, Error: e.Part.State.Error})
		if e.Part.State.Status != "completed" && e.Part.State.Status != "error" {
			return fmt.Errorf("unfinished tool event %s", e.Part.ID)
		}
	case "reasoning":
		// Reasoning is charged by provider usage; its text isn't a final answer.
	default:
		return fmt.Errorf("unsupported OpenCode event %q", e.Type)
	}
	return nil
}

func (log *eventLog) finish(e *event) error {
	if log.finishes[e.Part.Message] {
		return fmt.Errorf("multiple usage records for message %s", e.Part.Message)
	}
	if e.Part.Tokens == nil || e.Part.Tokens.Tokens() <= 0 {
		return fmt.Errorf("message %s lacks provider token usage", e.Part.Message)
	}
	u := *e.Part.Tokens
	if u.Input < 0 || u.Output < 0 || u.Reasoning < 0 || u.Cache.Read < 0 || u.Cache.Write < 0 || (u.Total != nil && *u.Total != u.Tokens()) {
		return fmt.Errorf("message %s has inconsistent token buckets", e.Part.Message)
	}
	log.trace.Usage.Add(u)
	log.trace.Turns++
	log.finishes[e.Part.Message] = true
	if e.Part.Reason == "stop" {
		log.finalMessage = e.Part.Message
	}
	return nil
}
