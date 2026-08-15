// demarkus-plugin is the shared core for every demarkus plugin (Claude Code, pi,
// Codex, …). Each plugin is a thin per-harness adapter: it normalizes its
// harness's hook/event into this binary's JSON contract, invokes a subcommand,
// and translates the JSON decision back. Centralizing the logic here means a fix
// lands once for all harnesses instead of being reimplemented per plugin.
//
// Usage:
//
//	echo '{"tool":"...","input":{...},"cwd":"..."}' | demarkus-plugin gate
//	demarkus-plugin version
//
// Subcommands: gate, nudge, guidance, update-check, registry, provision, mcp-serve.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/gate"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/guidance"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/nudge"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/update"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "gate":
		cmdGate()
	case "nudge":
		cmdNudge()
	case "guidance":
		cmdGuidance()
	case "update-check":
		cmdUpdateCheck()
	case "registry":
		cmdRegistry(os.Args[2:])
	case "mcp-serve":
		cmdMcpServe(os.Args[2:])
	case "provision":
		cmdProvision(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "usage: demarkus-plugin <command>\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  gate      Decide whether a mark_publish/mark_append should proceed (reads JSON on stdin)\n")
	fmt.Fprintf(os.Stderr, "  nudge     Decide a recall/promote/session-end nudge (reads JSON on stdin)\n")
	fmt.Fprintf(os.Stderr, "  guidance  Emit the session-start context for a surface (memory|knowledge)\n")
	fmt.Fprintf(os.Stderr, "  update-check  Report whether a newer release of the calling plugin exists\n")
	fmt.Fprintf(os.Stderr, "  version   Print version and exit\n")
}

// cmdNudge reads a nudge request as JSON on stdin and emits the reminder.
// --format json (default) → {"nudge":...}; claude → the event's hookSpecificOutput
// (recall→UserPromptSubmit, promote→PostToolUse additionalContext; session-end→
// a Stop decision:block). Empty nudge → no output. Fails silent (no nudge).
func cmdNudge() {
	fs := flag.NewFlagSet("nudge", flag.ExitOnError)
	format := fs.String("format", "json", "output format: json | claude")
	event := fs.String("event", "", "override event: recall | promote | session-end")
	surface := fs.String("surface", "", "override surface: memory | knowledge")
	changed := fs.Bool("changed-files", false, "session-end: the session changed files")
	soulWrite := fs.Bool("soul-write", false, "session-end: a soul write happened")
	_ = fs.Parse(os.Args[2:])

	var in nudge.Input
	raw, _ := io.ReadAll(os.Stdin)
	if strings.TrimSpace(string(raw)) != "" {
		if err := json.Unmarshal(raw, &in); err != nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] nudge: parse input: "+err.Error())
			return
		}
	}
	// Flag overrides let bash adapters pipe a raw Claude payload (for prompt /
	// tool_input) while setting the event/surface/booleans without building JSON.
	// Only applied when explicitly passed, so a pi JSON payload's values win otherwise.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["event"] {
		in.Event = *event
	}
	if set["surface"] {
		in.Surface = *surface
	}
	if set["changed-files"] {
		in.ChangedFiles = *changed
	}
	if set["soul-write"] {
		in.SoulWrite = *soulWrite
	}

	out, err := nudge.Evaluate(&in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] nudge: evaluate: "+err.Error())
		return
	}
	if out.Nudge == "" {
		return // nothing to surface
	}
	if *format == "claude" {
		switch in.Event {
		case "session-end":
			printJSON(map[string]any{"decision": "block", "reason": out.Nudge})
		case "promote":
			printJSON(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PostToolUse", "additionalContext": out.Nudge,
			}})
		default: // recall
			printJSON(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "UserPromptSubmit", "additionalContext": out.Nudge,
			}})
		}
		return
	}
	printJSON(out)
}

// cmdUpdateCheck reports whether a newer release of the calling plugin exists:
// {"message":...}, no output otherwise. The adapter supplies its own release
// identity (only it knows its channel); fails silent on any error.
func cmdUpdateCheck() {
	fs := flag.NewFlagSet("update-check", flag.ExitOnError)
	plugin := fs.String("plugin", "", "calling plugin's package name")
	installed := fs.String("installed", "", "version the plugin is running")
	manifestURL := fs.String("manifest-url", "", "URL of the published JSON manifest carrying {\"version\":...}")
	updateCommand := fs.String("update-command", "", "what the user runs to update this plugin")
	_ = fs.Parse(os.Args[2:])

	out, err := update.Evaluate(update.Input{
		Plugin:        *plugin,
		Installed:     *installed,
		ManifestURL:   *manifestURL,
		UpdateCommand: *updateCommand,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] update-check: "+err.Error())
		return
	}
	if out.Message == "" {
		return
	}
	printJSON(out)
}

// cmdGuidance emits the session-start context for a surface. --format json
// (default) → {"context":...}; claude → a SessionStart additionalContext payload.
// Empty context → no output.
func cmdGuidance() {
	fs := flag.NewFlagSet("guidance", flag.ExitOnError)
	surface := fs.String("surface", "memory", "memory | knowledge")
	guidanceFile := fs.String("guidance-file", "", "path to the plugin's static guidance markdown")
	format := fs.String("format", "json", "output format: json | claude")
	_ = fs.Parse(os.Args[2:])

	out, err := guidance.Evaluate(guidance.Input{Surface: *surface, GuidanceFile: *guidanceFile})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] guidance: "+err.Error())
		return
	}
	if out.Context == "" {
		return
	}
	if *format == "claude" {
		printJSON(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "SessionStart", "additionalContext": out.Context,
		}})
		return
	}
	printJSON(out)
}

// cmdGate reads a tool call as JSON on stdin (native {tool,input,cwd} or a Claude
// hook payload) and writes a decision. --format selects the output shape so the
// per-harness adapters stay zero-logic:
//
//	json        (default) → {"decision":...,"reason":...}        (pi reads this)
//	claude-pre            → Claude PreToolUse hookSpecificOutput  (deny/ask, else empty)
//	claude-post           → Claude PostToolUse hookSpecificOutput (warn → additionalContext)
//
// FAILS OPEN on any internal error (no output / allow) so a transient fault never
// blocks a legitimate write; the error is logged to stderr.
func cmdGate() {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	format := fs.String("format", "json", "output format: json | claude-pre | claude-post")
	_ = fs.Parse(os.Args[2:])

	fail := func(msg string) {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: "+msg+"; deferring")
		if *format == "json" {
			fmt.Println(`{"decision":"allow"}`)
		}
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(fmt.Sprintf("read stdin: %v", err))
		return
	}
	var in gate.Input
	if err := json.Unmarshal(raw, &in); err != nil {
		fail(fmt.Sprintf("parse input: %v", err))
		return
	}
	d, err := gate.Evaluate(in)
	if err != nil {
		fail(fmt.Sprintf("evaluate: %v", err))
		return
	}
	emitDecision(d, *format)
}

func emitDecision(d gate.Decision, format string) {
	switch format {
	case "claude-pre":
		// Block/ask are enforced before the call; warn is deferred to PostToolUse.
		var pd string
		switch d.Decision {
		case "block":
			pd = "deny"
		case "ask":
			pd = "ask"
		default:
			return // allow/warn → no Pre output
		}
		printJSON(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": pd, "permissionDecisionReason": d.Reason,
		}})
	case "claude-post":
		if d.Decision != "warn" {
			return // block/ask already handled Pre; allow → nothing
		}
		printJSON(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PostToolUse", "additionalContext": "⚠️ " + d.Reason,
		}})
	default: // json
		printJSON(d)
	}
}

func printJSON(v any) {
	out, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: marshal: "+err.Error())
		return
	}
	fmt.Println(string(out))
}
