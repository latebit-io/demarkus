// Package gate is the unified write-time gate shared by every demarkus plugin.
// One implementation of the publish tag-gate, the destination (binding) gate,
// and the knowledge required-axes/required-fields gate — so a fix lands once for
// all harnesses. The decision (allow|warn|block|ask) is harness-agnostic; each
// adapter maps it (e.g. pi has no native "ask", so it treats ask like block).
package gate

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol/store"
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
// must be non-blank; a number or bool counts as present; an array or object
// must be NON-EMPTY (an empty `[]`/`{}` is treated as missing, same as a blank
// string), so e.g. metadata.authors: ["ada"] is present but metadata.authors: []
// is not.
func fieldPresent(md map[string]any, key string) bool {
	if md == nil {
		return false
	}
	v, ok := md[key]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true // numbers, bools, etc. count as present
	}
}

// prunableRetention returns the normalized retention value when metadata
// carries one the server will accept — a positive integer, the only form that
// deletes versions (store.ParseRetention is the same predicate the server
// enforces). Server-rejectable values (0, negative, fractional, non-numeric)
// don't prompt: that write fails with bad-request and deletes nothing. JSON
// numbers are accepted alongside strings because the gate sees the raw tool
// input, before the MCP layer coerces metadata values to strings.
func prunableRetention(md map[string]any) (string, bool) {
	if md == nil {
		return "", false
	}
	v, ok := md["retention"]
	if !ok || v == nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		if n, ok := store.ParseRetention(strings.TrimSpace(x)); ok {
			return strconv.Itoa(n), true
		}
	case json.Number:
		if n, ok := store.ParseRetention(x.String()); ok {
			return strconv.Itoa(n), true
		}
	case float64:
		if x == math.Trunc(x) && x >= 1 && x <= math.MaxInt32 {
			return strconv.Itoa(int(x)), true
		}
	case int:
		// Programmatic callers (not JSON decode) can hand the gate a native
		// int; it reaches the server as its decimal string and prunes, so a
		// missing case here would be a silent-allow hole, not a safe default.
		if x >= 1 {
			return strconv.Itoa(x), true
		}
	case int64:
		if x >= 1 {
			return strconv.FormatInt(x, 10), true
		}
	}
	return "", false
}

// retentionDecision is the retention guard, independent of the tag and
// destination gates: a publish or append whose metadata carries a prunable
// retention value permanently deletes older versions, so the agent must
// confirm with the user before the write goes through. mark_graph_publish is
// exempt by construction — its verb parses as "graph_publish", and it sets
// retention by design on a generated document.
func retentionDecision(pt config.ParsedTool, args map[string]any) (*Decision, error) {
	if pt.Verb != "publish" && pt.Verb != "append" {
		return nil, nil
	}
	r, prunes := prunableRetention(metadataOf(args))
	if !prunes {
		return nil, nil
	}
	target := urlOr(args, "this document")
	reason := fmt.Sprintf(
		"demarkus write to %s sets metadata.retention=%s: the server will permanently delete all but the "+
			"newest %s versions of this document — on this write and every later write carrying the key. "+
			"This is intended for generated documents (graph exports, indexes). Confirm with the user before "+
			"proceeding; if history matters, re-issue the write without the retention key. To relax this "+
			"check, set DEMARKUS_RETENTION_STRICTNESS=warn.",
		target, r, r)
	s, err := config.RetentionStrictness()
	if err != nil {
		return nil, err
	}
	return &Decision{Decision: string(s), Reason: reason}, nil
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

func evalMemory(_ string, pt config.ParsedTool, args map[string]any, cwd, soulID string) (Decision, error) {
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

	// Retention guard (publish + append): destructive version pruning.
	rd, err := retentionDecision(pt, args)
	if err != nil {
		return Decision{}, err
	}
	if rd != nil {
		decisions = append(decisions, *rd)
	}

	// Documentation-style guard (publish only): body-shape rules.
	sd, err := styleDecision(pt, args)
	if err != nil {
		return Decision{}, err
	}
	if sd != nil {
		decisions = append(decisions, *sd)
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
	// Retention and style guards apply on their own verbs; the tag/axis gate
	// is publish-only. All are independent — combine to the most severe outcome.
	var decisions []Decision
	rd, err := retentionDecision(pt, args)
	if err != nil {
		return Decision{}, err
	}
	if rd != nil {
		decisions = append(decisions, *rd)
	}
	sd, err := styleDecision(pt, args)
	if err != nil {
		return Decision{}, err
	}
	if sd != nil {
		decisions = append(decisions, *sd)
	}
	if pt.Verb == "publish" {
		td, err := knowledgeTagDecision(args, slug)
		if err != nil {
			return Decision{}, err
		}
		if td != nil {
			decisions = append(decisions, *td)
		}
	}
	return combine(decisions), nil
}

// knowledgeTagDecision is the knowledge-system publish tag/axis/field gate:
// tags present, importance in range, required tag axes and required OKF
// metadata fields from the system's policy. Returns nil when compliant.
func knowledgeTagDecision(args map[string]any, slug string) (*Decision, error) {
	md := metadataOf(args)
	tags := strings.TrimSpace(tagsString(md))
	tagsOK := tags != ""
	impOK := importanceOK(md)
	url := urlOf(args)

	var missingAxes []string
	if tagsOK {
		axes, err := config.KnowledgeRequireTags(slug)
		if err != nil {
			return nil, err
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
		return nil, err
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
		return nil, nil
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
			"missing required OKF metadata fields: %s (set each as a key in the metadata object, e.g. metadata: {%q: \"...\"})",
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
		return nil, err
	}
	return &Decision{Decision: string(s), Reason: reason}, nil
}

func urlOr(args map[string]any, fallback string) string {
	if u := urlOf(args); u != "" {
		return u
	}
	return fallback
}
