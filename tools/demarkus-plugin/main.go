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
// Subcommands: gate, nudge, guidance, update-check, registry, provision, mcp-serve, doctor.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/gate"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/guidance"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
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
	case "doctor":
		cmdDoctor(os.Args[2:])
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
	fmt.Fprintf(os.Stderr, "  doctor    Audit a store for catalog hygiene and print the report (read-only)\n")
	fmt.Fprintf(os.Stderr, "  registry  Manage joined stores, bindings and promote targets; `registry project` prints a project's slug, store and binding state\n")
	fmt.Fprintf(os.Stderr, "  version   Print version and exit\n")
}

var sentinelSafeRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// readNudgeInput decodes the hook payload; empty input is a zero Input.
func readNudgeInput(r io.Reader) (nudge.Input, error) {
	var in nudge.Input
	raw, err := io.ReadAll(r)
	if err != nil {
		return in, fmt.Errorf("read input: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return in, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("parse input: %w", err)
	}
	return in, nil
}

// cmdNudge reads a nudge request as JSON on stdin and emits the reminder in
// the adapter's --format shape (json | claude | cursor). Empty nudge → no
// output; fails silent.
func cmdNudge() {
	fs := flag.NewFlagSet("nudge", flag.ExitOnError)
	format := fs.String("format", "json", "output format: json | claude | cursor")
	event := fs.String("event", "", "override event: recall | promote | session-end")
	surface := fs.String("surface", "", "override surface: memory | knowledge")
	changed := fs.Bool("changed-files", false, "session-end: the session changed files")
	memoryWrite := fs.Bool("memory-write", false, "session-end: a memory write happened")
	fs.BoolVar(memoryWrite, "soul-write", false, "deprecated alias of -memory-write")
	_ = fs.Parse(os.Args[2:]) // ExitOnError: Parse exits, never returns an error

	in, err := readNudgeInput(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] nudge: "+err.Error())
		return
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
	if set["memory-write"] || set["soul-write"] {
		in.MemoryWrite = *memoryWrite
	}

	h, hosted := host.ForHook(*format)
	// A once-per-session nudge is a sentinel keyed on session_id in the temp dir.
	var sentinel string
	if hosted && h.NudgeOnce && in.Event == "session-end" && in.SessionID != "" {
		sentinel = filepath.Join(os.TempDir(), "demarkus-memory-nudge-"+sentinelSafeRe.ReplaceAllString(in.SessionID, "_"))
		if _, err := os.Stat(sentinel); err == nil {
			return
		}
	}
	out, err := nudge.Evaluate(&in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] nudge: evaluate: "+err.Error())
		return
	}
	if out.Nudge == "" {
		return // nothing to surface
	}
	if sentinel != "" {
		// O_EXCL: concurrent Stops race to one nudge; a pre-existing path or
		// symlink in the shared temp dir is refused, not truncated.
		f, err := os.OpenFile(sentinel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if os.IsExist(err) {
				return
			}
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] nudge: sentinel: "+err.Error())
		} else if err := f.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] nudge: sentinel close: "+err.Error())
		}
	}
	if hosted {
		printJSON(h.Nudge(in.Event, out.Nudge))
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
	fs.Parse(os.Args[2:]) //nolint:errcheck // ExitOnError reports and exits

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
// (default) → {"context":...}; claude → a SessionStart additionalContext payload;
// cursor → a sessionStart additional_context payload. Empty context → no output.
func cmdGuidance() {
	fs := flag.NewFlagSet("guidance", flag.ExitOnError)
	surface := fs.String("surface", "memory", "memory | knowledge")
	guidanceFile := fs.String("guidance-file", "", "path to the plugin's static guidance markdown")
	projectDir := fs.String("project-dir", "", "project directory for the slug and binding header (default: harness environment)")
	format := fs.String("format", "json", "output format: json | claude | cursor")
	_ = fs.Parse(os.Args[2:]) // ExitOnError: Parse exits, never returns an error

	out, err := guidance.Evaluate(guidance.Input{Surface: *surface, GuidanceFile: *guidanceFile, ProjectDir: *projectDir})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] guidance: "+err.Error())
		return
	}
	if out.Context == "" {
		return
	}
	if h, ok := host.ForHook(*format); ok {
		printJSON(h.Guidance(out.Context))
		return
	}
	printJSON(out)
}

// cmdGate reads a tool call (native {tool,input,cwd}, Claude, or Cursor hook
// payload) from stdin and writes the decision in the adapter's --format shape.
// FAILS OPEN on any internal error so a transient fault never blocks a write.
func cmdGate() {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	format := fs.String("format", "json", "output format: json {decision,reason} | claude-pre (deny/ask) | claude-post (warn) | cursor (permission; warn = allow + agent_message)")
	_ = fs.Parse(os.Args[2:]) // ExitOnError: Parse exits, never returns an error

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
	d, err := gate.Evaluate(&in)
	if err != nil {
		fail(fmt.Sprintf("evaluate: %v", err))
		return
	}
	emitDecision(d, *format)
}

func emitDecision(d gate.Decision, format string) {
	h, phase, ok := host.ForGate(format)
	if !ok {
		printJSON(d) // json
		return
	}
	if out, emit := h.Gate(phase, d.Decision, d.Reason); emit {
		printJSON(out)
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
