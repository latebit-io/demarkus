// Package gate applies harness-independent write guards for every plugin.
// Adapters map allow, warn, block, and ask decisions to native behavior.
package gate

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Input accepts native {tool,input,cwd} and Claude hook payloads.
// The pi-mcp-adapter proxy shape is unwrapped downstream.
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

// leafOf returns the basename of a document URL or path.
func leafOf(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
}

// navExempt reports whether leaf is an OKF navigation/history file
// (index.md, log.md). These are exempt from the H1 rule and the required
// OKF `type` field: the server never defaults their type, and a hub is
// intentionally untyped navigation, not a concept document.
func navExempt(leaf string) bool {
	return leaf == "index.md" || leaf == "log.md"
}

// prunableRetention recognizes positive integers accepted by the server.
// Rejected values cannot prune and must not trigger a destructive-write prompt.
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
			"newest %s versions of this document, on this write and every later write carrying the key. "+
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
	sd, err := styleDecision(pt, args, styleRefMemory)
	if err != nil {
		return Decision{}, err
	}
	if sd != nil {
		decisions = append(decisions, *sd)
	}

	// PUBLISH checks complete metadata; APPEND checks only explicit overrides.
	if pt.Verb == "publish" || pt.Verb == "append" {
		md := metadataOf(args)
		result := publishpolicy.Evaluate(publishpolicy.Policy{}, urlOf(args), md)
		if pt.Verb == "append" {
			result = publishpolicy.EvaluateOverrides(publishpolicy.Policy{}, urlOf(args), md)
		}
		if !result.Compliant() {
			var problems []string
			if result.Has(publishpolicy.MissingTags) {
				problems = append(problems, "no metadata.tags (it will be invisible to mark_lookup)")
			}
			if result.Has(publishpolicy.InvalidImportance) {
				problems = append(problems, "metadata.importance outside [0,1]")
			}
			target := urlOr(args, "the document")
			reason := fmt.Sprintf(
				"demarkus %s to %s has %s. Re-issue mark_%s with a metadata object: tags "+
					"(comma-separated subjects derived from the content) and, if set, importance in [0,1]. "+
					"Tags are what make this document findable via mark_lookup.",
				pt.Verb, target, strings.Join(problems, "; "), pt.Verb)
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
	// Each guard selects its own verbs; combine independent outcomes by severity.
	var decisions []Decision
	rd, err := retentionDecision(pt, args)
	if err != nil {
		return Decision{}, err
	}
	if rd != nil {
		decisions = append(decisions, *rd)
	}
	sd, err := styleDecision(pt, args, styleRefKnowledge)
	if err != nil {
		return Decision{}, err
	}
	if sd != nil {
		decisions = append(decisions, *sd)
	}
	if pt.Verb == "publish" || pt.Verb == "append" {
		td, err := knowledgeTagDecision(args, slug, pt.Verb)
		if err != nil {
			return Decision{}, err
		}
		if td != nil {
			decisions = append(decisions, *td)
		}
	}
	return combine(decisions), nil
}

// knowledgeTagDecision evaluates complete publishes or explicit append overrides.
func knowledgeTagDecision(args map[string]any, slug, verb string) (*Decision, error) {
	md := metadataOf(args)
	url := urlOf(args)
	policy, err := config.KnowledgePolicy(slug)
	if err != nil {
		return nil, err
	}
	result := publishpolicy.Evaluate(policy, url, md)
	if verb == "append" {
		result = publishpolicy.EvaluateOverrides(policy, url, md)
	}
	if result.Compliant() {
		return nil, nil
	}

	missingAxes := result.Names(publishpolicy.MissingTagAxis)
	missingFields := result.Names(publishpolicy.MissingField)
	var problems []string
	if result.Has(publishpolicy.MissingTags) {
		problems = append(problems, "no metadata.tags (it will be invisible to mark_lookup)")
	}
	if result.Has(publishpolicy.InvalidImportance) {
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
		"demarkus %s to %s (knowledge system '%s') has %s. Re-issue mark_%s with a metadata object: "+
			"tags (comma-separated subjects derived from the content) and, if set, importance in [0,1]. "+
			"Tags are what make this document findable via mark_lookup across the shared catalog.",
		verb, target, slug, strings.Join(problems, "; "), verb)
	return &Decision{Decision: string(policy.EffectiveStrictness()), Reason: reason}, nil
}

func urlOr(args map[string]any, fallback string) string {
	if u := urlOf(args); u != "" {
		return u
	}
	return fallback
}
