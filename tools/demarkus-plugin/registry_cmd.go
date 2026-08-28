package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry"
)

// cmdProvision runs the server lifecycle (binary install, config, managed server,
// token, seed). Replaces setup.sh / provision.sh / the lib.sh lifecycle.
func cmdProvision(args []string) {
	// Hand the binary's own release version to provision so it fetches the token
	// from the tools release it shipped in (and never re-downloads itself).
	provision.Version = version
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
	case "status":
		printLine(provision.Status())
	case "health":
		printLine(provision.HealthWarning())
	case "detect":
		printLine(provision.DetectServers())
	case "verify-auth":
		printLine(provision.VerifyAuth())
	case "init":
		// Parse flags and the positional mode in ANY order. Go's flag package
		// stops at the first non-flag token, so it can't handle
		// `provision init reuse --port N`; and a naive "first non-flag = mode"
		// scan wrongly grabs a flag's value (`--port 6310 reuse` → mode "6310").
		// So walk the args ourselves, knowing --port/--root each take a value
		// (as `--flag V` or `--flag=V`); the first bare token is the mode.
		mode := ""
		port := 0
		root := ""
		rest := args[1:]
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
		if err := provision.Init(mode, port, root); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: provisioned (mode=" + mode + ")")
	default:
		fail("provision: unknown subcommand '" + args[0] + "' (use: status|health|detect|verify-auth|init)")
	}
}

// fail prints an operator-readable error (FAIL: on stdout so a slash command can
// echo it verbatim, plus stderr) and exits non-zero.
func fail(msg string) {
	fmt.Println("FAIL: " + msg)
	fmt.Fprintln(os.Stderr, "[demarkus-plugin] error: "+msg)
	os.Exit(1)
}

// cmdRegistry dispatches the registry mutations that replace the per-plugin bash
// (soul-join/soul-default/register-knowledge/mirror-policy/promote-target) and
// the mcp-config.mjs JS.
func cmdRegistry(args []string) {
	if len(args) == 0 {
		fail("registry: missing subcommand (mcp|soul-join|soul-default|knowledge-join|knowledge-register|knowledge-list|knowledge-unregister|policy-mirror|promote-target|detect-promote)")
	}
	switch args[0] {
	case "mcp":
		registryMcp(args[1:])
	case "soul-join":
		registrySoulJoin(args[1:])
	case "soul-default":
		registrySoulDefault(args[1:])
	case "knowledge-join":
		registryKnowledgeJoin(args[1:])
	case "knowledge-register":
		if len(args) != 2 {
			fail("knowledge-register: usage: registry knowledge-register <slug>")
		}
		if err := registry.KnowledgeRegister(args[1]); err != nil {
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
		existed, err := registry.KnowledgeUnregister(args[1])
		if err != nil {
			fail(err.Error())
		}
		if existed {
			fmt.Println("OK: unregistered knowledge system '" + args[1] + "'")
		} else {
			fmt.Println("OK: knowledge system '" + args[1] + "' was not registered")
		}
	case "policy-mirror":
		if len(args) != 2 {
			fail("policy-mirror: usage: registry policy-mirror <slug> (policy body on stdin)")
		}
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			fail("policy-mirror: read stdin: " + err.Error()) // never mirror a partial read — it would clear enforcement
		}
		if err := registry.PolicyMirror(args[1], string(body)); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: mirrored policy for '" + args[1] + "'")
	case "promote-target":
		registryPromoteTarget(args[1:])
	case "detect-promote":
		rows, err := registry.DetectPromote()
		if err != nil {
			fail(err.Error())
		}
		if len(rows) == 0 {
			fmt.Println("NONE")
			return
		}
		fmt.Println(strings.Join(rows, "\n"))
	default:
		fail("registry: unknown subcommand '" + args[0] + "'")
	}
}

func registryMcp(args []string) {
	if len(args) == 0 {
		fail("mcp: usage: registry mcp add|add-http|remove|list ...")
	}
	switch args[0] {
	case "add":
		if len(args) < 3 {
			fail("mcp add: usage: registry mcp add <name> <command> [args...]")
		}
		if err := registry.McpAdd(args[1], args[2], args[3:]); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: registered MCP server '" + args[1] + "'")
	case "add-http":
		if len(args) != 3 {
			fail("mcp add-http: usage: registry mcp add-http <name> <url>")
		}
		if err := registry.McpAddHTTP(args[1], args[2]); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: registered HTTP MCP server '" + args[1] + "'")
	case "remove":
		if len(args) != 2 {
			fail("mcp remove: usage: registry mcp remove <name>")
		}
		existed, err := registry.McpRemove(args[1])
		if err != nil {
			fail(err.Error())
		}
		if existed {
			fmt.Println("OK: removed MCP server '" + args[1] + "'")
		} else {
			fmt.Println("OK: no MCP server named '" + args[1] + "' (nothing to remove)")
		}
	case "list":
		names, err := registry.McpList()
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

func registrySoulJoin(args []string) {
	opts, err := parseSoulJoinArgs(args, os.Stdin)
	if err != nil {
		fail(err.Error())
	}
	res, err := registry.SoulJoin(opts.host, opts.token, opts.insecure, opts.bind)
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

type soulJoinOptions struct {
	host     string
	token    string
	insecure bool
	bind     string
}

const maxSoulJoinTokenInput = 4096

func parseSoulJoinArgs(args []string, stdin io.Reader) (soulJoinOptions, error) {
	var opts soulJoinOptions
	tokenStdin := false
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if positionalOnly {
			if opts.host != "" {
				return soulJoinOptions{}, fmt.Errorf("soul-join: unexpected extra argument %q", arg)
			}
			opts.host = arg
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		handled, err := parseSoulJoinFlag(args, &i, &opts, &tokenStdin)
		if err != nil {
			return soulJoinOptions{}, err
		}
		if handled {
			continue
		}
		if opts.host == "" {
			opts.host = arg
		} else {
			return soulJoinOptions{}, fmt.Errorf("soul-join: unexpected extra argument %q", arg)
		}
	}
	if opts.host == "" {
		return soulJoinOptions{}, errors.New("soul-join: usage: registry soul-join <host> [--token T|--token-stdin] [--insecure] [--bind DIR]")
	}
	if tokenStdin && opts.token != "" {
		return soulJoinOptions{}, errors.New("soul-join: --token and --token-stdin are mutually exclusive")
	}
	if tokenStdin {
		body, err := io.ReadAll(io.LimitReader(stdin, maxSoulJoinTokenInput+1))
		if err != nil {
			return soulJoinOptions{}, fmt.Errorf("soul-join: read token from stdin: %w", err)
		}
		if len(body) > maxSoulJoinTokenInput {
			return soulJoinOptions{}, fmt.Errorf("soul-join: token from stdin exceeds %d bytes", maxSoulJoinTokenInput)
		}
		opts.token = strings.TrimSpace(string(body))
		if opts.token == "" {
			return soulJoinOptions{}, errors.New("soul-join: token from stdin is empty")
		}
	}
	return opts, nil
}

func parseSoulJoinFlag(args []string, i *int, opts *soulJoinOptions, tokenStdin *bool) (bool, error) {
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
				return true, fmt.Errorf("soul-join: --%s wants a boolean, got %q", name, inline)
			}
			value = parsed
		}
		if name == "token-stdin" {
			*tokenStdin = value
		} else {
			opts.insecure = value
		}
	case "token", "bind":
		value, err := soulJoinFlagValue(args, i, name, inline, hasInline)
		if err != nil {
			return true, err
		}
		if name == "token" {
			if value == "" {
				return true, errors.New("soul-join: token is empty")
			}
			opts.token = value
		} else {
			opts.bind = value
		}
	default:
		return true, fmt.Errorf("soul-join: unknown flag %q", arg)
	}
	return true, nil
}

func soulJoinFlagValue(args []string, i *int, name, inline string, hasInline bool) (string, error) {
	if hasInline {
		return inline, nil
	}
	*i++
	if *i >= len(args) {
		return "", fmt.Errorf("soul-join: --%s requires a value", name)
	}
	return args[*i], nil
}

func registrySoulDefault(args []string) {
	fs := flag.NewFlagSet("soul-default", flag.ExitOnError)
	list := fs.Bool("list", false, "list the catalog")
	set := fs.String("set", "", "set DIR's default soul to SLUG")
	bind := fs.String("bind", "", "the project directory")
	_ = fs.Parse(args)
	if *bind == "" {
		fail("soul-default: --bind DIR is required")
	}
	current, err := config.ProjectBinding(*bind)
	if err != nil {
		fail(err.Error())
	}
	switch {
	case *list:
		cat, err := registry.SoulCatalog()
		if err != nil {
			fail(err.Error())
		}
		if len(cat) == 0 {
			fmt.Println("EMPTY")
			return
		}
		fmt.Println("CATALOG")
		for _, row := range cat {
			id := strings.SplitN(row, "\t", 2)[0]
			marker := "-"
			if id == current {
				marker = "*"
			}
			fmt.Println(row + "\t" + marker)
		}
	case *set != "":
		if err := registry.ProjectBindSet(*bind, *set); err != nil {
			fail(err.Error())
		}
		fmt.Println("OK " + *set)
	default:
		fail("soul-default: choose --list or --set SLUG")
	}
}

func registryKnowledgeJoin(args []string) {
	if len(args) != 1 {
		fail("knowledge-join: usage: registry knowledge-join <broker-url>")
	}
	res, err := registry.ValidateBrokerEndpoint(args[0])
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
		rows, err := registry.PromoteTargetList()
		if err != nil {
			fail(err.Error())
		}
		fmt.Println(strings.Join(rows, "\n"))
	case "add":
		if len(args) < 3 {
			fail("promote-target add: usage: registry promote-target add <slug> <path> [label]")
		}
		label := strings.Join(args[3:], " ")
		path, err := registry.PromoteTargetAdd(args[1], args[2], label)
		if err != nil {
			fail(err.Error())
		}
		fmt.Println("OK: registered promote target " + args[1] + " " + path)
	default:
		fail("promote-target: unknown subcommand '" + args[0] + "'")
	}
}

// cmdMcpServe launches demarkus-mcp for the local soul (default) or a joined
// remote soul (--soul <slug>), reading host/insecure/token from the shared state
// and injecting DEMARKUS_AUTH. Replaces mcp-wrapper.sh + soul-remote-wrapper.sh.
func cmdMcpServe(args []string) {
	fs := flag.NewFlagSet("mcp-serve", flag.ExitOnError)
	soul := fs.String("soul", "", "joined remote-soul slug (omit for the local managed soul)")
	_ = fs.Parse(args)

	binDir, err := config.StatePath("bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: "+err.Error())
		os.Exit(1)
	}
	mcpBin := filepath.Join(binDir, "demarkus-mcp")
	if _, err := os.Stat(mcpBin); err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: demarkus-mcp not installed at "+mcpBin+"; run /soul-init")
		os.Exit(1)
	}

	var host, tokenFile string
	insecure := false
	if *soul == "" {
		cfg, err := config.LoadConfig()
		if err != nil || cfg == nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: no local soul configured; SessionStart may not have run yet")
			os.Exit(1)
		}
		host = "mark://localhost:" + cfg.Port
		insecure = true
		tf, _ := config.StatePath("plugin-memory.token")
		tokenFile = tf
	} else {
		row, ok, err := registry.RemoteSoulRow(*soul)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: catalog lookup failed: "+err.Error())
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: soul '"+*soul+"' not in the catalog; re-run /soul-join")
			os.Exit(1)
		}
		host, insecure, tokenFile = row.Host, row.Insecure, row.TokenFile
		if row.IsBroker() {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: soul '"+*soul+"' is a broker soul served over HTTP MCP; register it with `claude mcp add --transport http` instead of mcp-serve")
			os.Exit(1)
		}
	}

	env := os.Environ()
	if tokenFile != "" && tokenFile != "-" {
		tok, err := os.ReadFile(tokenFile)
		if err == nil && strings.TrimSpace(string(tok)) != "" {
			env = append(env, "DEMARKUS_AUTH="+strings.TrimSpace(string(tok)))
		} else if *soul != "" {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: token file "+tokenFile+" missing/empty; re-run /soul-join --token")
			os.Exit(1)
		}
	}

	argv := []string{mcpBin, "-host", host}
	if insecure {
		argv = append(argv, "-insecure")
	}
	argv = append(argv, fs.Args()...) // forward any extra args the harness appends
	if err := syscall.Exec(mcpBin, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] mcp-serve: exec failed: "+err.Error())
		os.Exit(1)
	}
}
