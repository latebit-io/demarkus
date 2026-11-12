// Package gate applies harness-independent write guards for every plugin.
// Adapters map allow, warn, block, and ask decisions to native behavior.
package gate

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/links"
	docgate "github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/client/memory/promote"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/taint"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/toolname"
	"github.com/mark3labs/mcp-go/mcp"
)

// Input accepts native {tool,input,cwd}, Claude, and Cursor hook payloads.
// The pi-mcp-adapter proxy shape is unwrapped downstream.
type Input struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
	Cwd   string         `json:"cwd"`
	// Claude and Cursor hook payload fields (used when Tool/Input are absent).
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	// Cursor reports the server apart from the tool and the project as
	// workspace_roots instead of cwd.
	McpServerName  string   `json:"mcp_server_name"`
	WorkspaceRoots []string `json:"workspace_roots"`
	// TranscriptPath is Claude's session JSONL, scanned for untrusted reads.
	TranscriptPath string `json:"transcript_path"`
	// ToolUseID is Claude's id of the call, the same for every hook on it.
	ToolUseID string `json:"tool_use_id"`
	// SessionID is Claude's session, the scope of a warning shown once.
	SessionID string `json:"session_id"`
	// Taint lists untrusted reads an adapter that sees every tool tracked
	// itself (OpenCode, pi).
	Taint []string `json:"taint"`
	// Post: a post-call hook, which shows only a warn.
	Post bool `json:"-"`
}

// Decision is the gate verdict. Reason is plain; adapters add any presentation
// (e.g. a ⚠️ prefix for warn).
type Decision struct {
	Decision string `json:"decision"` // allow | warn | block | ask
	Reason   string `json:"reason,omitempty"`
	// UpdatedInput is the tool input with the taint stamp, for a host hook
	// that can rewrite a call it asks about; never on the native wire.
	UpdatedInput map[string]any `json:"-"`
	// A warn that includes the taint warning: sources are the reads it names,
	// rest the other warnings in Reason ("" for none).
	sources []string
	rest    string
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
		if n, ok := storefmt.ParseRetention(strings.TrimSpace(x)); ok {
			return strconv.Itoa(n), true
		}
	case json.Number:
		if n, ok := storefmt.ParseRetention(x.String()); ok {
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

// retentionDecision is the retention guard: a store write carrying a prunable
// retention permanently deletes older versions, so the user confirms it.
// mark_graph_publish is exempt: it sets retention by design.
func retentionDecision(pt toolname.Parsed, args map[string]any) (*Decision, error) {
	if !writes(pt, args) || pt.Verb == "graph_publish" {
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
	s, err := retentionStrictness()
	if err != nil {
		return nil, err
	}
	return &Decision{Decision: string(s), Reason: reason}, nil
}

// governanceDecision flags a knowledge write under /.well-known/demarkus/:
// the policy, template and style guide bind every writer, so the change is
// surfaced (warn by default, ask or block by the knob).
func governanceDecision(pt toolname.Parsed, args map[string]any) (*Decision, error) {
	url := urlOf(args)
	if !writes(pt, args) || !governanceURL(url) {
		return nil, nil
	}
	s, err := governanceStrictness()
	if err != nil {
		return nil, err
	}
	return &Decision{Decision: string(s), Reason: fmt.Sprintf(
		"demarkus %s to %s changes a governance document, which every writer of the knowledge system follows. "+
			"Review the change with the user; DEMARKUS_GOVERNANCE_STRICTNESS adjusts this check (warn/ask/block).",
		pt.Verb, url)}, nil
}

// writes reports a store-write call that sends a write, not a report.
func writes(pt toolname.Parsed, args map[string]any) bool {
	return pt.StoreWrite() && !reportOnly(pt, args)
}

// reportOnly is a curate or promote call that writes nothing: it builds the
// proposal or packet a later call applies. Apply is read as the server reads it.
func reportOnly(pt toolname.Parsed, args map[string]any) bool {
	switch pt.Verb {
	case "curate":
		req := mcp.CallToolRequest{}
		req.Params.Arguments = args
		return !req.GetBool("apply", false)
	case "promote":
		phase, _ := args["phase"].(string)
		packet, _ := args["packet"].(string)
		return phase == promote.PhaseDryRun || phase == promote.PhaseStage || (phase == promote.PhaseRefresh && packet == "")
	}
	return false
}

// governanceURL reports a mark:// URL or bare path naming a governance document.
func governanceURL(raw string) bool {
	if t, err := links.ParseMark(raw); err == nil {
		return storefmt.IsGovernancePath(t.Path)
	}
	return storefmt.IsGovernancePath(raw)
}

// contentScanner runs the rules the soul server runs: secrets always, personal
// data without work domains (a store names its own people).
var contentScanner = contentcheck.New(contentcheck.Options{PII: contentcheck.Warn})

// contentDecision scans the body of a store write before it leaves. A secret
// blocks with the fix (an ask has no answerer in a headless run); personal
// data and text addressed to an agent warn. No knob: the server judges too.
func contentDecision(pt toolname.Parsed, args map[string]any) *Decision {
	if !writes(pt, args) {
		return nil
	}
	body, _ := args["body"].(string)
	findings := contentScanner.Scan(body)
	if len(findings) == 0 {
		return nil
	}
	target := urlOr(args, "the document")
	if contentcheck.Secrets(findings) {
		return &Decision{Decision: "block", Reason: fmt.Sprintf(
			"demarkus %s to %s carries a secret: %s. Fix: %s.", pt.Verb, target, contentcheck.Summary(findings), contentcheck.Remedy(findings))}
	}
	return &Decision{Decision: "warn", Reason: fmt.Sprintf(
		"demarkus %s to %s carries %s: %s. Fix: %s.", pt.Verb, target, contentcheck.Describe(findings), contentcheck.Summary(findings), contentcheck.Remedy(findings))}
}

// taintDecision escalates a write after an untrusted read, where a planted
// instruction would act; an unreadable transcript asks. No scan when the
// verdict cannot show: a block stands, or a post hook drops an ask.
func taintDecision(pt toolname.Parsed, args map[string]any, in *Input, d Decision) (*Decision, []string, error) {
	if !pt.StoreWrite() || d.Decision == "block" {
		return nil, nil, nil
	}
	s, err := taintStrictness()
	if err != nil || s == Off || (in.Post && s != Warn) {
		return nil, nil, err
	}
	var found []string
	if in.TranscriptPath != "" {
		if found, err = taint.Transcript(in.TranscriptPath); err != nil {
			unreadable := Ask
			if in.Post {
				// A post hook shows only a warn; an ask would drop every warning.
				unreadable = Warn
			}
			return &Decision{Decision: string(unreadable), Reason: fmt.Sprintf(
				"demarkus %s: the session transcript could not be read (%v), so untrusted reads are unknown; approve only a write you reviewed.", pt.Verb, err)}, []string{"unreadable transcript"}, nil
		}
	}
	sources := taint.Merge(in.Taint, found)
	if len(sources) == 0 {
		return nil, nil, nil
	}
	return &Decision{Decision: string(s), Reason: fmt.Sprintf(
		"demarkus %s to %s after this session read untrusted content (%s): text planted there may be steering this write. Approve only if you reviewed what it writes; to change this check, set DEMARKUS_TAINT_STRICTNESS to ask, warn or off.",
		pt.Verb, urlOr(args, "the store"), strings.Join(sources, "; "))}, sources, nil
}

// stamped is a publish's input with the taint label in its metadata; other
// verbs carry no metadata, so nil (an append inherits a stamped base).
func stamped(pt toolname.Parsed, input map[string]any, sources []string) map[string]any {
	if pt.Verb != "publish" || input == nil {
		return nil
	}
	meta := maps.Clone(metadataOf(input))
	if meta == nil {
		meta = map[string]any{}
	}
	meta[protocol.TaintKey] = protocol.TaintValue
	meta[protocol.TaintFromKey] = strings.Join(sources, "; ")
	out := maps.Clone(input)
	out["metadata"] = meta
	return out
}

// Evaluate applies the right gate(s) for the tool's target and returns the
// decision. Memory (memory) and knowledge surfaces are mutually exclusive by scope.
func Evaluate(in *Input) (Decision, error) {
	// Accept the Claude hook shape (tool_name/tool_input) when the native fields
	// are absent.
	if in.Tool == "" && in.ToolName != "" {
		in.Tool = in.ToolName
		in.Input = in.ToolInput
	}
	in.Tool = toolname.Qualify(in.Tool, in.McpServerName)
	tool, args := host.NormalizeCall(in.Tool, in.Input)
	pt, ok := toolname.Parse(tool)
	if !ok {
		return allow(), nil
	}

	d, ours, err := route(pt, tool, args, in)
	if err != nil || !ours {
		return d, err
	}
	t, sources, err := taintDecision(pt, args, in, d)
	if err != nil || t == nil {
		return d, err
	}
	combined := combine([]Decision{d, *t})
	if combined.Decision == "warn" && t.Decision == "warn" {
		combined.sources = sources
		if d.Decision == "warn" {
			combined.rest = d.Reason
		}
	}
	// The proxy shape (tool "mcp") wraps its args; only a direct call is rewritten.
	if combined.Decision == "ask" && tool == in.Tool {
		combined.UpdatedInput = stamped(pt, in.Input, sources)
	}
	return combined, nil
}

// route runs the target's own guards; ours is false (allow) for a server the
// gate does not hold.
func route(pt toolname.Parsed, tool string, args map[string]any, in *Input) (d Decision, ours bool, err error) {
	memoryID, err := toolname.MemoryTargetID(tool)
	if err != nil {
		return Decision{}, false, err
	}
	if memoryID != "" {
		cwd, ambiguous := in.Cwd, ""
		if cwd == "" {
			if cwd, ambiguous, err = projectDir(in.WorkspaceRoots); err != nil {
				return Decision{}, false, err
			}
		}
		d, err = evalMemory(pt, args, cwd, memoryID, ambiguous)
		return d, true, err
	}
	ksSlug, err := toolname.KnowledgeScope(tool)
	if err != nil || ksSlug == "" {
		return allow(), false, err // unrelated server — not ours to gate
	}
	d, err = evalKnowledge(pt, args, ksSlug)
	return d, true, err
}

// projectDir resolves the binding-check project when the payload has no cwd.
// workspace_roots has no active-root order: roots agreeing on one binding
// resolve to it, differing bindings come back ambiguous, no roots → env var.
func projectDir(roots []string) (cwd, ambiguous string, err error) {
	var found []string
	for _, r := range roots {
		if r != "" {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return host.ProjectDir(), "", nil
	case 1:
		return found[0], "", nil
	}
	bindings := map[string]string{} // slug → first root bound to it
	var order []string
	for _, r := range found {
		bound, err := config.ProjectBinding(r)
		if err != nil {
			return "", "", err
		}
		if bound == "" {
			continue
		}
		if _, seen := bindings[bound]; !seen {
			bindings[bound] = r
			order = append(order, bound)
		}
	}
	switch len(order) {
	case 0:
		return "", "", nil
	case 1:
		return bindings[order[0]], "", nil
	}
	return "", fmt.Sprintf("this workspace has several roots bound to different memories (%s) and the write carries no cwd, so the project cannot be determined",
		strings.Join(order, ", ")), nil
}

func evalMemory(pt toolname.Parsed, args map[string]any, cwd, memoryID, ambiguous string) (Decision, error) {
	// The guards are independent: evaluate every one and return the most
	// severe outcome (reasons at that severity combined), so a `warn` misroute
	// never hides a `block` from the tag gate.
	var decisions []Decision

	// Destination gate (publish + append): misroute against the project binding.
	// An ambiguous multi-root workspace is gated at the same strictness rather
	// than guessed, since the caller fails open on errors.
	if (pt.Verb == "publish" || pt.Verb == "append") && ambiguous != "" {
		s, err := memoryDestStrictness()
		if err != nil {
			return Decision{}, err
		}
		decisions = append(decisions, Decision{Decision: string(s), Reason: fmt.Sprintf(
			"demarkus write to %s: %s. Open a single workspace root, or run /soul-default so the roots agree; to relax this check, set DEMARKUS_MEMORY_DEST_STRICTNESS=warn (or ask).",
			urlOr(args, "this document"), ambiguous)})
	} else if pt.Verb == "publish" || pt.Verb == "append" {
		bound, err := config.ProjectBinding(cwd)
		if err != nil {
			return Decision{}, err
		}
		if bound != "" && bound != memoryID {
			target := urlOr(args, "this document")
			reason := fmt.Sprintf(
				"demarkus write to %s is going to store '%s', but this project is bound to store '%s'. "+
					"Re-issue the write against the bound store's tools (the %s MCP server's mark_publish / mark_append). "+
					"To change which store this project uses, run /soul-default in this repo; to relax this check, set "+
					"DEMARKUS_MEMORY_DEST_STRICTNESS=warn (or ask).",
				target, memoryID, bound, bound)
			s, err := memoryDestStrictness()
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
	if cd := contentDecision(pt, args); cd != nil {
		decisions = append(decisions, *cd)
	}

	// Documentation-style guard (publish only): body-shape rules.
	sd, err := styleDecision(pt, args, docgate.Baseline)
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
			problems = append(problems, unpinnedProblem(result)...)
			target := urlOr(args, "the document")
			reason := fmt.Sprintf(
				"demarkus %s to %s has %s. Re-issue mark_%s with a metadata object: tags "+
					"(comma-separated subjects derived from the content) and, if set, importance in [0,1]. "+
					"Tags are what make this document findable via mark_lookup.",
				pt.Verb, target, strings.Join(problems, "; "), pt.Verb)
			s, err := memoryTagStrictness()
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

func evalKnowledge(pt toolname.Parsed, args map[string]any, slug string) (Decision, error) {
	// Each guard selects its own verbs; combine independent outcomes by severity.
	var decisions []Decision
	rd, err := retentionDecision(pt, args)
	if err != nil {
		return Decision{}, err
	}
	if rd != nil {
		decisions = append(decisions, *rd)
	}
	if cd := contentDecision(pt, args); cd != nil {
		decisions = append(decisions, *cd)
	}
	gd, err := governanceDecision(pt, args)
	if err != nil {
		return Decision{}, err
	}
	if gd != nil {
		decisions = append(decisions, *gd)
	}
	sd, err := styleDecision(pt, args, docgate.KnowledgeGuide)
	if err != nil {
		return Decision{}, err
	}
	if sd != nil {
		decisions = append(decisions, *sd)
	}
	if pt.Verb == "publish" || pt.Verb == "append" {
		if td := knowledgeTagDecision(args, slug, pt.Verb); td != nil {
			decisions = append(decisions, *td)
		}
	}
	return combine(decisions), nil
}

// knowledgeTagDecision checks tags and importance on complete publishes or
// explicit append overrides; the server enforces the system's policy.
func knowledgeTagDecision(args map[string]any, slug, verb string) *Decision {
	md := metadataOf(args)
	url := urlOf(args)
	policy := publishpolicy.Policy{Strictness: knowledgeStrictness()}
	result := publishpolicy.Evaluate(policy, url, md)
	if verb == "append" {
		result = publishpolicy.EvaluateOverrides(policy, url, md)
	}
	if result.Compliant() {
		return nil
	}

	var problems []string
	if result.Has(publishpolicy.MissingTags) {
		problems = append(problems, "no metadata.tags (it will be invisible to mark_lookup)")
	}
	if result.Has(publishpolicy.InvalidImportance) {
		problems = append(problems, "metadata.importance outside [0,1]")
	}
	problems = append(problems, unpinnedProblem(result)...)
	target := url
	if target == "" {
		target = "the document"
	}
	reason := fmt.Sprintf(
		"demarkus %s to %s (knowledge system '%s') has %s. Re-issue mark_%s with a metadata object: "+
			"tags (comma-separated subjects derived from the content) and, if set, importance in [0,1]. "+
			"Tags are what make this document findable via mark_lookup across the shared catalog.",
		verb, target, slug, strings.Join(problems, "; "), verb)
	return &Decision{Decision: string(policy.EffectiveStrictness()), Reason: reason}
}

// unpinnedProblem words the rel-based-on values that name no version.
func unpinnedProblem(result publishpolicy.Result) []string {
	names := result.Names(publishpolicy.UnpinnedSource)
	if len(names) == 0 {
		return nil
	}
	return []string{provenance.Key + " values that are no versioned permalink (" + strings.Join(names, ", ") + "; pin " + provenance.PinForms + ")"}
}

func urlOr(args map[string]any, fallback string) string {
	if u := urlOf(args); u != "" {
		return u
	}
	return fallback
}
