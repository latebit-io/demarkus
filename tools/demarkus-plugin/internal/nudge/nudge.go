// Package nudge is the shared soft-nudge logic for every demarkus plugin: the
// recall reminder, the ADR promote nudge, and the session-end journal nudge.
// All non-blocking — each returns reminder text or nothing; the adapter decides
// how to surface it (Claude additionalContext / Stop-block, pi sendMessage /
// notify).
package nudge

import (
	"regexp"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Input is the union of fields the three nudge events need; an adapter fills the
// ones relevant to its event.
type Input struct {
	Event   string         `json:"event"`   // recall | promote | session-end
	Surface string         `json:"surface"` // memory | knowledge (recall only)
	Prompt  string         `json:"prompt"`
	Tool    string         `json:"tool"`
	Input   map[string]any `json:"input"`
	// session-end signals (computed by the adapter: Claude from the transcript,
	// pi from in-session activity tracking).
	ChangedFiles bool `json:"changedFiles"`
	MemoryWrite  bool `json:"memoryWrite"`
	// LegacyMemoryWrite is the pre-rename key older adapters still send.
	LegacyMemoryWrite bool `json:"soulWrite"`
	// Claude hook payload fields (used when Tool/Input are absent) so an adapter
	// can pipe the raw Claude payload verbatim. UserPromptSubmit carries `prompt`
	// (already mapped above); PostToolUse carries tool_name/tool_input.
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	// Cursor reports the MCP server apart from the tool name.
	McpServerName string `json:"mcp_server_name"`
}

// Output carries the nudge text, empty when nothing should be surfaced.
type Output struct {
	Nudge string `json:"nudge,omitempty"`
}

// Recall-intent patterns. Multi-word and specific so ordinary questions don't
// trip them. The knowledge variant adds org/team/standard phrasings.
var memoryRecallRe = regexp.MustCompile(`\bdid we\b|\bhave we\b|\bhad we\b|\bwhat did we\b|\bwhy did we\b|\bhow did we\b|\bwhat( is| was|s)? our\b|\bdo we (have|know)\b|\bwe (decided|agreed|chose|discussed|already)\b|\blast time\b|\bpreviously\b|\brecall\b|\bis there (a |an |any )?(adr|decision|note|record)\b`)
var knowledgeRecallRe = regexp.MustCompile(memoryRecallRe.String() + `|\bwhat does the (org|team|company)\b|\b(our|team|org|company|agreed|approved) (standard|convention)s?\b`)

var adrRe = regexp.MustCompile(`/adr/.*\.md$`)

const memoryRecallNudge = "This reads like a recall question. Before answering from this conversation alone, check the memory first: mark_lookup the project for the subject (and mark_fetch the project /index.md), then answer from what's recorded; or say plainly if nothing is there."

const knowledgeRecallNudge = "This reads like a recall question. If it concerns shared or organizational knowledge, check the joined demarkus knowledge system first: mark_lookup the system for the subject (and fetch the relevant world's index.md / the root hub), then answer from what's recorded; or say plainly if nothing is there."

const journalNudge = "This session changed files but recorded nothing to the memory. If something here is worth remembering (a decision, a gotcha, a non-obvious why), capture it with /memory-journal (or mark_append to today's journal under /<project>/journal/<YYYY-MM-DD>.md). If it's all routine, ignore this."

// Evaluate dispatches on the event and returns the nudge text (or empty).
func Evaluate(in *Input) (Output, error) {
	in.MemoryWrite = in.MemoryWrite || in.LegacyMemoryWrite
	// Accept the Claude PostToolUse shape (tool_name/tool_input) for promote.
	if in.Tool == "" && in.ToolName != "" {
		in.Tool = in.ToolName
		in.Input = in.ToolInput
	}
	in.Tool = config.QualifyTool(in.Tool, in.McpServerName)
	switch in.Event {
	case "recall":
		return recall(in)
	case "promote":
		return promote(in)
	case "session-end":
		return sessionEnd(in)
	default:
		return Output{}, nil
	}
}

func recall(in *Input) (Output, error) {
	lc := strings.ToLower(in.Prompt)
	if in.Surface == "knowledge" {
		present, err := config.KnowledgePresent()
		if err != nil {
			return Output{}, err
		}
		if present && knowledgeRecallRe.MatchString(lc) {
			return Output{Nudge: knowledgeRecallNudge}, nil
		}
		return Output{}, nil
	}
	// default: the local memory
	present, err := config.LocalMemoryPresent()
	if err != nil {
		return Output{}, err
	}
	if present && memoryRecallRe.MatchString(lc) {
		return Output{Nudge: memoryRecallNudge}, nil
	}
	return Output{}, nil
}

func promote(in *Input) (Output, error) {
	tool, args := config.NormalizeCall(in.Tool, in.Input)
	pt, ok := config.ParseTool(tool)
	if !ok || pt.Verb != "publish" {
		return Output{}, nil
	}
	present, err := config.PromoteDestinationPresent()
	if err != nil {
		return Output{}, err
	}
	if !present {
		return Output{}, nil
	}
	local, err := config.PublishGateScopeLocal(tool)
	if err != nil {
		return Output{}, err
	}
	if !local {
		return Output{}, nil
	}
	url, _ := args["url"].(string)
	if !adrRe.MatchString(url) {
		return Output{}, nil
	}
	body, _ := args["body"].(string)
	if strings.Contains(body, "promoted: mark://") {
		return Output{}, nil // already back-stamped → don't re-nudge
	}
	return Output{Nudge: "New ADR published to the memory (" + url + "). ADRs are high-signal, shared-team knowledge: if this decision is durable and useful beyond you, /promote " + url + " to lift it into the knowledge system (curate → gate → publish). If it is personal or provisional, leave it here; most memory content correctly stays put."}, nil
}

func sessionEnd(in *Input) (Output, error) {
	present, err := config.LocalMemoryPresent()
	if err != nil {
		return Output{}, err
	}
	if present && in.ChangedFiles && !in.MemoryWrite {
		return Output{Nudge: journalNudge}, nil
	}
	return Output{}, nil
}
