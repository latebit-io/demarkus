// Package gate is the unified write-time gate shared by every demarkus plugin.
// One implementation of the publish tag-gate, the destination (binding) gate,
// and the knowledge required-axes/required-fields gate — so a fix lands once for
// all harnesses. The decision (allow|warn|block|ask) is harness-agnostic; each
// adapter maps it (e.g. pi has no native "ask", so it treats ask like block).
package gate

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Input is the tool call an adapter passes through. It accepts two shapes so the
// bash adapters can pipe a harness payload verbatim with no JSON munging:
//   - native:      {tool, input, cwd}                       (pi, generic)
//   - Claude hook: {tool_name, tool_input, cwd, ...}        (Claude Code hooks)
//
// The pi-mcp-adapter "mcp" proxy shape is unwrapped downstream.
type Input struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
	Cwd   string         `json:"cwd"`
	// Claude hook payload fields (used when Tool/Input are absent).
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

// Decision is the gate verdict. Reason is plain; adapters add any presentation
// (e.g. a ⚠️ prefix for warn).
type Decision struct {
	Decision string `json:"decision"` // allow | warn | block | ask
	Reason   string `json:"reason,omitempty"`
}

func allow() Decision { return Decision{Decision: "allow"} }

func metadataOf(args map[string]any) map[string]any {
	if m, ok := args["metadata"].(map[string]any); ok {
		return m
	}
	return nil
}

func urlOf(args map[string]any) string {
	if u, ok := args["url"].(string); ok {
		return u
	}
	return ""
}

func tagsString(md map[string]any) string {
	if md == nil {
		return ""
	}
	if t, ok := md["tags"].(string); ok {
		return t
	}
	return ""
}

// fieldPresent reports whether a required metadata field is satisfied. A string
// must be non-blank; any other non-null value (number, bool, array, object) is
// accepted as present — only string fields have an emptiness notion, so e.g.
// metadata.authors: ["ada"] must not read as missing.
func fieldPresent(md map[string]any, key string) bool {
	if md == nil {
		return false
	}
	v, ok := md[key]
	if !ok || v == nil {
		return false
	}
	if s, isStr := v.(string); isStr {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// importanceOK: absent is fine; otherwise a number or non-blank numeric string in
// [0,1]. Blank string, array, object, bool are rejected (not coerced to 0).
func importanceOK(md map[string]any) bool {
	if md == nil {
		return true
	}
	v, present := md["importance"]
	if !present || v == nil {
		return true
	}
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return false
		}
		n = f
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return false
		}
		n = f
	default:
		return false
	}
	return n >= 0 && n <= 1
}

// Evaluate applies the right gate(s) for the tool's target and returns the
// decision. Memory (soul) and knowledge surfaces are mutually exclusive by scope.
func Evaluate(in Input) (Decision, error) {
	// Accept the Claude hook shape (tool_name/tool_input) when the native fields
	// are absent.
	if in.Tool == "" && in.ToolName != "" {
		in.Tool = in.ToolName
		in.Input = in.ToolInput
	}
	tool, args := config.NormalizeCall(in.Tool, in.Input)
	pt, ok := config.ParseTool(tool)
	if !ok {
		return allow(), nil
	}

	soulID, err := config.SoulTargetID(tool)
	if err != nil {
		return Decision{}, err
	}
	if soulID != "" {
		return evalMemory(tool, pt, args, in.Cwd, soulID)
	}

	ksSlug, err := config.KnowledgeScope(tool)
	if err != nil {
		return Decision{}, err
	}
	if ksSlug != "" {
		return evalKnowledge(pt, args, ksSlug)
	}

	return allow(), nil // unrelated server — not ours to gate
}

func evalMemory(tool string, pt config.ParsedTool, args map[string]any, cwd, soulID string) (Decision, error) {
	// The destination gate and the publish tag-gate are INDEPENDENT (in Claude
	// Code they're two separate hooks) — evaluate both and return the most severe
	// outcome, so e.g. a misroute set to `warn` can't suppress a `block` from the
	// tag gate. Reasons at the winning severity are combined.
	var decisions []Decision

	// Destination gate (publish + append): misroute against the project binding.
	if pt.Verb == "publish" || pt.Verb == "append" {
		bound, err := config.ProjectBinding(cwd)
		if err != nil {
			return Decision{}, err
		}
		if bound != "" && bound != soulID {
			target := urlOr(args, "this document")
			reason := fmt.Sprintf(
				"demarkus write to %s is going to soul '%s', but this project is bound to soul '%s'. "+
					"Re-issue the write against the bound soul's tools (the %s MCP server's mark_publish / mark_append). "+
					"To change which soul this project uses, run /soul-join in this repo; to relax this check, set "+
					"DEMARKUS_MEMORY_DEST_STRICTNESS=warn (or ask).",
				target, soulID, bound, bound)
			s, err := config.MemoryDestStrictness()
			if err != nil {
				return Decision{}, err
			}
			decisions = append(decisions, Decision{Decision: string(s), Reason: reason})
		}
	}

	// Publish tag-gate (publish only): missing tags or out-of-range importance.
	if pt.Verb == "publish" {
		md := metadataOf(args)
		tagsOK := strings.TrimSpace(tagsString(md)) != ""
		impOK := importanceOK(md)
		if !tagsOK || !impOK {
			var problems []string
			if !tagsOK {
				problems = append(problems, "no metadata.tags (it will be invisible to mark_lookup)")
			}
			if !impOK {
				problems = append(problems, "metadata.importance outside [0,1]")
			}
			target := urlOr(args, "the document")
			reason := fmt.Sprintf(
				"demarkus publish to %s has %s. Re-issue mark_publish with a metadata object: tags "+
					"(comma-separated subjects derived from the content) and, if set, importance in [0,1]. "+
					"Tags are what make this document findable via mark_lookup.",
				target, strings.Join(problems, "; "))
			s, err := config.MemoryTagStrictness()
			if err != nil {
				return Decision{}, err
			}
			decisions = append(decisions, Decision{Decision: string(s), Reason: reason})
		}
	}

	return combine(decisions), nil
}

// combine reduces independent gate outcomes to the most severe (block > ask >
// warn > allow), joining the reasons of all decisions at the winning severity.
func combine(ds []Decision) Decision {
	if len(ds) == 0 {
		return allow()
	}
	rank := map[string]int{"allow": 0, "warn": 1, "ask": 2, "block": 3}
	best := ds[0]
	for _, d := range ds[1:] {
		if rank[d.Decision] > rank[best.Decision] {
			best = d
		}
	}
	var reasons []string
	for _, d := range ds {
		if d.Decision == best.Decision && d.Reason != "" {
			reasons = append(reasons, d.Reason)
		}
	}
	best.Reason = strings.Join(reasons, " Also: ")
	return best
}

func evalKnowledge(pt config.ParsedTool, args map[string]any, slug string) (Decision, error) {
	if pt.Verb != "publish" {
		return allow(), nil
	}
	md := metadataOf(args)
	tags := strings.TrimSpace(tagsString(md))
	tagsOK := tags != ""
	impOK := importanceOK(md)
	url := urlOf(args)

	var missingAxes []string
	if tagsOK {
		axes, err := config.KnowledgeRequireTags(slug)
		if err != nil {
			return Decision{}, err
		}
		for _, axis := range axes {
			if !config.TagsHaveAxis(tags, axis) {
				missingAxes = append(missingAxes, axis)
			}
		}
	}

	var missingFields []string
	fields, err := config.KnowledgeRequireFields(slug)
	if err != nil {
		return Decision{}, err
	}
	leaf := url
	if i := strings.LastIndex(leaf, "/"); i >= 0 {
		leaf = leaf[i+1:]
	}
	for _, f := range fields {
		// index.md / log.md are server-exempt from the OKF `type` default only —
		// a hub is intentionally untyped. Every other required field is checked
		// generically: the binary has the full metadata, so any policy-declared
		// field is satisfied by a non-empty metadata.<field> (unlike the old bash
		// gate, which could only inspect `type` and silently skipped the rest).
		if f == "type" && (leaf == "index.md" || leaf == "log.md") {
			continue
		}
		if !fieldPresent(md, f) {
			missingFields = append(missingFields, f)
		}
	}

	if tagsOK && impOK && len(missingAxes) == 0 && len(missingFields) == 0 {
		return allow(), nil
	}

	var problems []string
	if !tagsOK {
		problems = append(problems, "no metadata.tags (it will be invisible to mark_lookup)")
	}
	if !impOK {
		problems = append(problems, "metadata.importance outside [0,1]")
	}
	if len(missingAxes) > 0 {
		problems = append(problems, fmt.Sprintf(
			"missing required tag axes for this knowledge system: %s (tag as \"axis:value\", e.g. %s:<value>)",
			strings.Join(missingAxes, " "), missingAxes[0]))
	}
	if len(missingFields) > 0 {
		problems = append(problems, fmt.Sprintf(
			"missing required OKF metadata fields: %s (set each as a key in the metadata object, e.g. metadata: {\"%s\": \"...\"})",
			strings.Join(missingFields, " "), missingFields[0]))
	}
	target := url
	if target == "" {
		target = "the document"
	}
	reason := fmt.Sprintf(
		"demarkus publish to %s (knowledge system '%s') has %s. Re-issue mark_publish with a metadata object: "+
			"tags (comma-separated subjects derived from the content) and, if set, importance in [0,1]. "+
			"Tags are what make this document findable via mark_lookup across the shared catalog.",
		target, slug, strings.Join(problems, "; "))
	s, err := config.KnowledgeStrictness(slug)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Decision: string(s), Reason: reason}, nil
}

func urlOr(args map[string]any, fallback string) string {
	if u := urlOf(args); u != "" {
		return u
	}
	return fallback
}
