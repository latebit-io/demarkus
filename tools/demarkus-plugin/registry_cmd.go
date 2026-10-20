package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/hosttools"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/broker"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/join"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/knowledge"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/mcpconfig"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/promote"
)

// cmdProvision runs the server lifecycle (binary install, config, managed server,
// token, seed). Replaces setup.sh / provision.sh / the lib.sh lifecycle.
func cmdProvision(args []string) {
	if len(args) == 0 {
		if err := provision.Provision(); err != nil {
			fail(err.Error())
		}
		return
	}
	// printLine reports a one-line result, failing the command on error.
	printLine := func(s string, err error) {
		if err != nil {
			fail(err.Error())
		}
		if s != "" {
			fmt.Println(s)
		}
	}
	switch args[0] {
	case "if-configured":
		if err := provision.IfConfigured(); err != nil {
			fail(err.Error())
		}
	case "status":
		printLine(provision.Status())
	case "health":
		printLine(provision.HealthWarning())
	case "detect":
		printLine(provision.DetectServers())
	case "verify-auth":
		printLine(provision.VerifyAuth())
	case "init":
		cmdProvisionInit(args[1:])
	default:
		fail("provision: unknown subcommand '" + args[0] + "' (use: if-configured|status|health|detect|verify-auth|init)")
	}
}

// fail prints an operator-readable error (FAIL: on stdout so a slash command can
// echo it verbatim, plus stderr) and exits non-zero.
func fail(msg string) {
	fmt.Println("FAIL: " + msg)
	fmt.Fprintln(os.Stderr, "[demarkus-plugin] error: "+msg)
	os.Exit(1)
}

// cmdRegistry dispatches the registry mutations that replaced the per-plugin
// bash and the mcp-config.mjs JS; the switch below is the subcommand list.
func cmdRegistry(args []string) {
	if len(args) == 0 {
		fail("registry: missing subcommand (mcp|memory-join|memory-default|knowledge-join|knowledge-register|knowledge-list|knowledge-unregister|promote-target|detect-promote|project)")
	}
	switch args[0] {
	case "mcp":
		registryMcp(args[1:])
	case "memory-join":
		registryMemoryJoin(args[1:])
	case "memory-default":
		registryMemoryDefault(args[1:])
	case "project":
		registryProject(args[1:])
	case "knowledge-join":
		registryKnowledgeJoin(args[1:])
	case "knowledge-register":
		if len(args) != 2 {
			fail("knowledge-register: usage: registry knowledge-register <slug>")
		}
		if err := knowledge.Register(args[1]); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: registered knowledge system '" + args[1] + "'")
	case "knowledge-list":
		systems, err := config.ListKnowledgeSystems()
		if err != nil {
			fail(err.Error())
		}
		for _, s := range systems {
			fmt.Println(s)
		}
	case "knowledge-unregister":
		if len(args) != 2 {
			fail("knowledge-unregister: usage: registry knowledge-unregister <slug>")
		}
		existed, err := knowledge.Unregister(args[1])
		if err != nil {
			fail(err.Error())
		}
		if existed {
			fmt.Println("OK: unregistered knowledge system '" + args[1] + "'")
		} else {
			fmt.Println("OK: knowledge system '" + args[1] + "' was not registered")
		}
	case "promote-target":
		registryPromoteTarget(args[1:])
	case "detect-promote":
		rows, err := promote.Detect()
		if err != nil {
			fail(err.Error())
		}
		fmt.Println(promote.Report(rows))
	default:
		fail("registry: unknown subcommand '" + args[0] + "'")
	}
}

// splitMcpHarness takes `--harness <h>` / `--harness=<h>` from the front of
// args only, so `mcp add <name> <cmd> ...` keeps every child argument intact.
func splitMcpHarness(args []string) (harness string, rest []string, err error) {
	for len(args) > 0 {
		switch {
		case args[0] == "--harness":
			if len(args) < 2 {
				return "", nil, errors.New("mcp: --harness requires a value (pi|cursor)")
			}
			harness = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--harness="):
			harness = strings.TrimPrefix(args[0], "--harness=")
			args = args[1:]
		default:
			return harness, args, nil
		}
	}
	return harness, args, nil
}

func registryMcp(args []string) {
	harness, args, err := splitMcpHarness(args)
	if err != nil {
		fail(err.Error())
	}
	h, err := host.ForMcp(harness)
	if err != nil {
		fail(err.Error())
	}
	if len(args) == 0 {
		fail("mcp: usage: registry mcp [--harness pi|cursor] add|add-http|remove|list ...")
	}
	switch args[0] {
	case "add":
		if len(args) < 3 {
			fail("mcp add: usage: registry mcp add <name> <command> [args...]")
		}
		if err := mcpconfig.Add(h, args[1], args[2], args[3:]); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: registered MCP server '" + args[1] + "'")
	case "add-http":
		if len(args) != 3 {
			fail("mcp add-http: usage: registry mcp add-http <name> <url>")
		}
		if err := mcpconfig.AddHTTP(h, args[1], args[2]); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: registered HTTP MCP server '" + args[1] + "'")
	case "remove":
		if len(args) != 2 {
			fail("mcp remove: usage: registry mcp remove <name>")
		}
		existed, err := mcpconfig.Remove(h, args[1])
		if err != nil {
			fail(err.Error())
		}
		if existed {
			fmt.Println("OK: removed MCP server '" + args[1] + "'")
		} else {
			fmt.Println("OK: no MCP server named '" + args[1] + "' (nothing to remove)")
		}
	case "list":
		names, err := mcpconfig.List(h)
		if err != nil {
			fail(err.Error())
		}
		if len(names) == 0 {
			fmt.Println("(no MCP servers configured)")
			return
		}
		fmt.Println(strings.Join(names, "\n"))
	default:
		fail("mcp: unknown subcommand '" + args[0] + "'")
	}
}

func registryMemoryJoin(args []string) {
	opts, err := parseMemoryJoinArgs(args, os.Stdin)
	if err != nil {
		fail(err.Error())
	}
	res, err := join.Memory(opts.host, opts.token, opts.insecure, opts.bind)
	if err != nil {
		fail(err.Error())
	}
	ins := "0"
	if res.Insecure {
		ins = "1"
	}
	fmt.Println("OK")
	fmt.Println("slug=" + res.Slug)
	fmt.Println("host=" + res.Host)
	fmt.Println("insecure=" + ins)
	fmt.Println("token-file=" + res.TokenFile)
	if res.Broker {
		fmt.Println("broker=1")
		fmt.Println("mcp-url=" + res.McpURL)
	}
}

type memoryJoinOptions struct {
	host     string
	token    string
	insecure bool
	bind     string
}

const maxMemoryJoinTokenInput = 4096

func parseMemoryJoinArgs(args []string, stdin io.Reader) (memoryJoinOptions, error) {
	var opts memoryJoinOptions
	tokenStdin := false
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if positionalOnly {
			if opts.host != "" {
				return memoryJoinOptions{}, fmt.Errorf("memory-join: unexpected extra argument %q", arg)
			}
			opts.host = arg
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		handled, err := parseMemoryJoinFlag(args, &i, &opts, &tokenStdin)
		if err != nil {
			return memoryJoinOptions{}, err
		}
		if handled {
			continue
		}
		if opts.host == "" {
			opts.host = arg
		} else {
			return memoryJoinOptions{}, fmt.Errorf("memory-join: unexpected extra argument %q", arg)
		}
	}
	if opts.host == "" {
		return memoryJoinOptions{}, errors.New("memory-join: usage: registry memory-join <host> [--token T|--token-stdin] [--insecure] [--bind DIR]")
	}
	if tokenStdin && opts.token != "" {
		return memoryJoinOptions{}, errors.New("memory-join: --token and --token-stdin are mutually exclusive")
	}
	if tokenStdin {
		body, err := io.ReadAll(io.LimitReader(stdin, maxMemoryJoinTokenInput+1))
		if err != nil {
			return memoryJoinOptions{}, fmt.Errorf("memory-join: read token from stdin: %w", err)
		}
		if len(body) > maxMemoryJoinTokenInput {
			return memoryJoinOptions{}, fmt.Errorf("memory-join: token from stdin exceeds %d bytes", maxMemoryJoinTokenInput)
		}
		opts.token = strings.TrimSpace(string(body))
		if opts.token == "" {
			return memoryJoinOptions{}, errors.New("memory-join: token from stdin is empty")
		}
	}
	return opts, nil
}

func parseMemoryJoinFlag(args []string, i *int, opts *memoryJoinOptions, tokenStdin *bool) (bool, error) {
	arg := args[*i]
	if arg == "-" || !strings.HasPrefix(arg, "-") {
		return false, nil
	}
	flagArg := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, inline, hasInline := strings.Cut(flagArg, "=")
	switch name {
	case "token-stdin", "insecure":
		value := true
		if hasInline {
			parsed, err := strconv.ParseBool(inline)
			if err != nil {
				return true, fmt.Errorf("memory-join: --%s wants a boolean, got %q", name, inline)
			}
			value = parsed
		}
		if name == "token-stdin" {
			*tokenStdin = value
		} else {
			opts.insecure = value
		}
	case "token", "bind":
		value, err := memoryJoinFlagValue(args, i, name, inline, hasInline)
		if err != nil {
			return true, err
		}
		if name == "token" {
			if value == "" {
				return true, errors.New("memory-join: token is empty")
			}
			opts.token = value
		} else {
			opts.bind = value
		}
	default:
		return true, fmt.Errorf("memory-join: unknown flag %q", arg)
	}
	return true, nil
}

func memoryJoinFlagValue(args []string, i *int, name, inline string, hasInline bool) (string, error) {
	if hasInline {
		return inline, nil
	}
	*i++
	if *i >= len(args) {
		return "", fmt.Errorf("memory-join: --%s requires a value", name)
	}
	return args[*i], nil
}

func registryMemoryDefault(args []string) {
	fs := flag.NewFlagSet("memory-default", flag.ExitOnError)
	list := fs.Bool("list", false, "list the catalog")
	set := fs.String("set", "", "set DIR's default memory to SLUG")
	bind := fs.String("bind", "", "the project directory")
	_ = fs.Parse(args) // ExitOnError: Parse never returns
	if *bind == "" {
		fail("memory-default: --bind DIR is required")
	}
	switch {
	case *list:
		listing, err := hosttools.Catalog(*bind)
		if err != nil {
			fail(err.Error())
		}
		if _, err := fmt.Println(listing); err != nil {
			fail("memory-default: write listing: " + err.Error())
		}
	case *set != "":
		out, err := hosttools.Default(*bind, *set)
		if err != nil {
			fail(err.Error())
		}
		fmt.Println(out)
	default:
		fail("memory-default: choose --list or --set SLUG")
	}
}

// registryProject prints the slug, store, root and verdict a prompt needs
// before its first mark_* call; mark_host project answers the same text.
func registryProject(args []string) {
	fs := flag.NewFlagSet("project", flag.ExitOnError)
	dir := fs.String("dir", "", "the project directory (default: harness project variable)")
	_ = fs.Parse(args) // ExitOnError: Parse never returns
	if len(fs.Args()) != 0 {
		fail("project: usage: registry project [--dir DIR]")
	}
	out, err := hosttools.Project(*dir)
	if err != nil {
		fail("project: " + err.Error())
	}
	if _, err := fmt.Println(out); err != nil {
		fail("project: write output: " + err.Error())
	}
}

func registryKnowledgeJoin(args []string) {
	if len(args) != 1 {
		fail("knowledge-join: usage: registry knowledge-join <broker-url>")
	}
	res, err := broker.Validate(args[0])
	if err != nil {
		fail(err.Error())
	}
	fmt.Println("OK")
	fmt.Println("url=" + res.URL)
	fmt.Println("slug=" + res.Slug)
	fmt.Println("mcp-url=" + res.McpURL)
}

func registryPromoteTarget(args []string) {
	if len(args) == 0 {
		fail("promote-target: usage: registry promote-target add <slug> <path> [label] | list")
	}
	switch args[0] {
	case "list":
		rows, err := config.PromoteTargets()
		if err != nil {
			fail(err.Error())
		}
		fmt.Println(strings.Join(rows, "\n"))
	case "add":
		if len(args) < 3 {
			fail("promote-target add: usage: registry promote-target add <slug> <path> [label]")
		}
		label := strings.Join(args[3:], " ")
		path, err := promote.AddTarget(args[1], args[2], label)
		if err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: registered promote target " + args[1] + " " + path)
	default:
		fail("promote-target: unknown subcommand '" + args[0] + "'")
	}
}

// setEnv replaces key in an execve environment; execve keeps the first
// duplicate, so an inherited value would otherwise win over ours.
func setEnv(env []string, key, value string) []string {
	return append(unsetEnv(env, key), key+"="+value)
}

// unsetEnv returns env without any entry for key.
func unsetEnv(env []string, keys ...string) []string {
	kept := env[:0:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(keys, name) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// toolProviderEnv names the binary demarkus-mcp loads host tools from; the
// environment, not a flag, so an older demarkus-mcp still starts.
const toolProviderEnv = "DEMARKUS_MCP_TOOL_PROVIDER"

// hostToolsEnv set non-empty in the harness's MCP config asks mcp-serve for
// the host tools without a flag an older plugin binary would reject.
const hostToolsEnv = "DEMARKUS_HOST_TOOLS"

// mcpServeEnv builds the demarkus-mcp environment. An inherited DEMARKUS_AUTH
// or provider is always dropped: it belongs to some other server, and exec
// keeps the first duplicate.
func mcpServeEnv(base []string, token, provider string) []string {
	env := unsetEnv(base, "DEMARKUS_AUTH", toolProviderEnv)
	if token != "" {
		env = append(env, "DEMARKUS_AUTH="+token)
	}
	if provider != "" {
		env = append(env, toolProviderEnv+"="+provider)
	}
	// Plugin prompts are checked against the lean profile. The environment
	// carries the choice so an older demarkus-mcp still starts.
	return setEnv(env, "DEMARKUS_MCP_PROFILE", "lean")
}

// cmdMcpServe launches demarkus-mcp for the local memory or a joined remote one
// (--memory <slug>), injecting DEMARKUS_AUTH from the shared state; --host-tools
// (or DEMARKUS_HOST_TOOLS) also loads this binary's host tools into the server.
func cmdMcpServe(args []string) {
	fs := flag.NewFlagSet("mcp-serve", flag.ExitOnError)
	memory := fs.String("memory", "", "joined remote-memory slug (omit for the local managed memory)")
	name := fs.String("name", "", "MCP server name a branded plugin registers the local memory under; recorded so gates resolve it")
	hostTools := fs.Bool("host-tools", false, "serve mark_host and mark_host_setup beside the store's tools (local managed memory only; "+hostToolsEnv+" set does the same)")
	_ = fs.Parse(args) // ExitOnError: Parse never returns

	// The env is a hint for the local server; exported shell-wide it must not
	// take a joined memory's server down, so it never applies with --memory.
	if *memory == "" && os.Getenv(hostToolsEnv) != "" {
		*hostTools = true
	}
	if *memory != "" && (*name != "" || *hostTools) {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: -name and -host-tools apply to the local managed memory only")
		os.Exit(2)
	}
	provider := ""
	if *hostTools {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: locate this binary for the host tools: "+err.Error())
			os.Exit(1)
		}
		provider = self
	}
	mcpBin, err := config.BinPath("demarkus-mcp")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: "+err.Error())
		os.Exit(1)
	}
	if _, err := os.Stat(mcpBin); err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: demarkus-mcp not installed at "+mcpBin+"; run /soul-init")
		os.Exit(1)
	}

	id := *memory
	if id == "" {
		id = config.LocalMemoryID
	}
	ep, err := catalog.Resolve(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: "+err.Error())
		os.Exit(1)
	}
	if ep.Broker {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: memory '"+id+"' is a broker memory served over HTTP MCP; register it with `claude mcp add --transport http` instead of mcp-serve")
		os.Exit(1)
	}
	env := mcpServeEnv(os.Environ(), ep.Token, provider)
	argv := []string{mcpBin, "-host", ep.Host}
	if ep.Insecure {
		argv = append(argv, "-insecure")
	}
	argv = append(argv, fs.Args()...) // forward any extra args the harness appends

	// Record the alias last, once startup can no longer fail short of exec: a
	// rejected name (one a joined store uses) must not start the server, and a
	// persisted name without a running server would strand the gates.
	previous, token, recorded := "", "", false
	if *memory == "" {
		alias, err := config.LocalMemoryAlias()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: read local memory alias: "+err.Error())
			os.Exit(1)
		}
		// An unbranded start with no alias on disk touches nothing: the default
		// name needs no record, and the write path stays off for plain installs.
		if recorded = *name != "" || alias != ""; recorded {
			if previous, token, err = catalog.SetLocalAlias(*name); err != nil {
				fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: record local memory alias: "+err.Error())
				os.Exit(1)
			}
		}
	}
	if err := syscall.Exec(mcpBin, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: exec failed: "+err.Error())
		if recorded {
			// Only while our write is still the stored one: a later mcp-serve may have started.
			if rerr := catalog.RestoreLocalAlias(previous, token); rerr != nil {
				fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: restore local memory alias: "+rerr.Error())
			}
		}
		os.Exit(1)
	}
}

// cmdProvisionInit parses `provision init` flags and the mode in any order.
// The flag package stops at the first non-flag token (`init reuse --port N`),
// so walk the args: --port/--root take a value, the first bare token is the mode.
func cmdProvisionInit(rest []string) {
	mode := ""
	port := 0
	root := ""
	takeVal := func(i *int, flagName string) string {
		if eq := strings.IndexByte(rest[*i], '='); eq >= 0 {
			return rest[*i][eq+1:]
		}
		*i++
		if *i >= len(rest) {
			fail("provision init: " + flagName + " requires a value")
		}
		return rest[*i]
	}
	isFlag := func(a, name string) bool {
		return a == "--"+name || a == "-"+name || strings.HasPrefix(a, "--"+name+"=") || strings.HasPrefix(a, "-"+name+"=")
	}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case isFlag(a, "port"):
			v := takeVal(&i, "--port")
			n, err := strconv.Atoi(v)
			if err != nil {
				fail("provision init: --port wants an integer, got '" + v + "'")
			}
			port = n
		case isFlag(a, "root"):
			root = takeVal(&i, "--root")
		case strings.HasPrefix(a, "-"):
			fail("provision init: unknown flag '" + a + "'")
		default:
			if mode != "" {
				fail("provision init: unexpected extra argument '" + a + "'")
			}
			mode = a
		}
	}
	if mode == "" {
		fail("provision init: requires a mode (default|reuse|isolated)")
	}
	out, err := hosttools.Init(mode, port, root)
	if err != nil {
		fail(err.Error())
	}
	fmt.Println(out)
}
