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
// Subcommands beyond `gate` (nudge, guidance, registry, provision, mcp-serve)
// are introduced in later phases.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/gate"
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
	fmt.Fprintf(os.Stderr, "  version   Print version and exit\n")
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
