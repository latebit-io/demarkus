package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/internal/bookmarks"
	"github.com/latebit-io/demarkus/client/internal/cache"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/client/joinurl"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

const quicBufferWarningEnv = "QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING"

func main() {
	if err := suppressQUICBufferWarning(); err != nil {
		log.Fatal(err)
	}
	ctx, stop := interruptContext()
	defer stop()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "edit":
			editMain(ctx, os.Args[2:])
			return
		case "graph":
			graphMain(ctx, os.Args[2:])
			return
		case "info":
			infoMain(ctx, os.Args[2:])
			return
		case "token":
			tokenMain(os.Args[2:])
			return
		case "join":
			joinMain(os.Args[2:])
			return
		case "bookmark":
			bookmarkMain(ctx, os.Args[2:])
			return
		case "lookup":
			lookupMain(ctx, os.Args[2:])
			return
		case "okf":
			okfMain(ctx, os.Args[2:])
			return
		}
	}
	requestMain(ctx)
}

// interruptContext ends on Ctrl-C so an in flight request is cancelled, not
// abandoned. A second Ctrl-C gets the default disposition and kills the process.
func interruptContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// Short-lived CLI processes cannot tune host sysctls and would emit quic-go's
// advisory on every command. An explicit environment setting still wins.
func suppressQUICBufferWarning() error {
	if _, explicit := os.LookupEnv(quicBufferWarningEnv); explicit {
		return nil
	}
	if err := os.Setenv(quicBufferWarningEnv, "true"); err != nil {
		return fmt.Errorf("set %s: %w", quicBufferWarningEnv, err)
	}
	return nil
}

func requestMain(ctx context.Context) {
	verb := flag.String("X", protocol.VerbFetch, "request verb (FETCH, LIST, VERSIONS, PUBLISH, ARCHIVE, APPEND)")
	body := flag.String("body", "", "request body (for PUBLISH/APPEND); reads stdin if omitted")
	authToken := flag.String("auth", "", "auth token for all requests including reads on private paths (env: DEMARKUS_AUTH)")
	expectedVersion := flag.Int("expected-version", 0, "version check: 0 create-only, N replace version N; required for PUBLISH unless -force; APPEND resolves it when omitted")
	force := flag.Bool("force", false, "PUBLISH only: overwrite whatever version is there, without a version check")
	onConflict := flag.String("on-conflict", "", "PUBLISH only: merge (default) prints a merged candidate to review, fail reports the bare conflict")
	meta := metaFlag{}
	flag.Var(meta, "meta", "publisher metadata key=value for PUBLISH/APPEND (repeatable); e.g. -meta tags=go,auth -meta importance=0.9")
	yes := flag.Bool("yes", false, "skip the confirmation prompt for destructive metadata (retention)")
	includeArchived := flag.Bool("include-archived", false, "LIST only: include archived documents (and all-archived directories) in the listing")
	listCursor := flag.String("cursor", "", "LIST only: opaque continuation cursor from a prior response")
	listPageSize := flag.Int("page-size", 0, "LIST only: maximum entries per page, 1-1000 (default: server choice)")
	verbose := flag.Bool("v", false, "show status and metadata header before body")
	noCache := flag.Bool("no-cache", false, "disable caching")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	cacheDir := flag.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus [-v] [-X VERB] [-body TEXT] [-auth TOKEN] mark://host:port/path\n")
		fmt.Fprintf(os.Stderr, "       demarkus edit [-auth TOKEN] [-insecure] mark://host:port/path.md\n")
		fmt.Fprintf(os.Stderr, "       demarkus graph [-depth N] [-insecure] mark://host:port/path\n")
		fmt.Fprintf(os.Stderr, "       demarkus info [-insecure] mark://host:port\n")
		fmt.Fprintf(os.Stderr, "       demarkus bookmark <add|list|remove>\n")
		fmt.Fprintf(os.Stderr, "       demarkus lookup -query SUBJECT [-filter K=V,...] [-limit N] [-match body] mark://host:port/scope/\n")
		fmt.Fprintf(os.Stderr, "       demarkus token <add|remove|list>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	*verb = strings.ToUpper(*verb)
	if err := validateVerb(*verb); err != nil {
		log.Fatal(err)
	}

	target := mustTarget(flag.Arg(0))
	host, path := target.DialHost(), target.Path

	opts := fetch.Options{Insecure: *insecure}
	if !*noCache {
		opts.Cache = cache.New(*cacheDir)
	}

	ts := tokens.LoadDefault()
	token := tokens.Resolve(tokens.Credential{Explicit: *authToken, Origin: host}, host, ts)
	// Confirm destructive metadata before resolveBody so the prompt runs while
	// stdin is still untouched (resolveBody may consume stdin for the body).
	// Only PUBLISH/APPEND transmit metadata, so only they can prune.
	if *verb == protocol.VerbPublish || *verb == protocol.VerbAppend {
		if err := confirmRetention(metaMap(meta), *yes, os.Stdin, os.Stderr); err != nil {
			log.Fatal(err)
		}
	}
	reqBody := resolveBody(*verb, *body)

	client := fetch.NewClient(opts)
	defer client.Close()

	if isWriteVerb(*verb) {
		outcome, err := runWrite(ctx, &cliWrite{
			verb: *verb, doc: directDoc(client, host, path, token), body: reqBody,
			expectedVersion: givenInt("expected-version", expectedVersion),
			force:           *force, onConflict: *onConflict, meta: metaMap(meta),
		})
		if err != nil {
			log.Fatal(err)
		}
		outcome.print(*verbose)
		if outcome.code != 0 {
			client.Close()
			os.Exit(outcome.code)
		}
		return
	}

	var (
		result fetch.Result
		err    error
	)
	switch *verb {
	case protocol.VerbFetch:
		result, err = client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: path, Token: token})
	case protocol.VerbList:
		result, err = client.List(ctx, fetch.ListRequest{
			Host: host, Path: path, Token: token,
			IncludeArchived: *includeArchived,
			Cursor:          *listCursor,
			PageSize:        *listPageSize,
		})
	case protocol.VerbVersions:
		result, err = client.Versions(ctx, fetch.VersionsRequest{Host: host, Path: path, Token: token})
	case protocol.VerbLookup:
		log.Fatal("use 'demarkus lookup -query SUBJECT mark://host/scope/' for LOOKUP requests")
	}
	if err != nil {
		log.Fatal(err)
	}

	printResult(result, *verbose)
}

func editMain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	authToken := fs.String("auth", "", "auth token (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	useCache := fs.Bool("cache", false, "enable caching (disabled by default for edit)")
	cacheDir := fs.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus edit [-auth TOKEN] [-insecure] mark://host:port/path.md\n\n")
		fmt.Fprintf(os.Stderr, "Fetch a document, open it in $EDITOR, and publish changes.\n")
		fmt.Fprintf(os.Stderr, "Creates a new document if it doesn't exist.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}

	target := mustTarget(fs.Arg(0))
	host, path := target.DialHost(), target.Path

	editorEnv := os.Getenv("EDITOR")
	editorFields := strings.Fields(editorEnv)
	if len(editorFields) == 0 {
		editorFields = []string{"vi"}
	}

	token := tokens.Resolve(tokens.Credential{Explicit: *authToken, Origin: host}, host, tokens.LoadDefault())

	opts := fetch.Options{Insecure: *insecure}
	if *useCache {
		opts.Cache = cache.New(*cacheDir)
	}
	client := fetch.NewClient(opts)
	defer client.Close()

	result, err := client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: path, Token: token})
	if err != nil {
		log.Fatal(err)
	}
	// Checked before the editor opens, so a document that cannot be edited costs no typing.
	if _, err := editedFrom(result.Response, ""); err != nil {
		log.Fatal(err)
	}
	original := result.Response.Body
	if result.Response.Status == protocol.StatusNotFound {
		original = ""
		fmt.Fprintf(os.Stderr, "Document not found, creating new document.\n")
	}

	// Write content to a temp file for editing.
	tmpFile, err := os.CreateTemp("", "demarkus-edit-*.md")
	if err != nil {
		log.Fatalf("create temp file: %v", err)
	}
	// Best effort: a leftover temp file holds only what the user just saw.
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.WriteString(original); err != nil {
		log.Fatalf("write temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		log.Fatalf("close temp file: %v", err)
	}

	// Open the editor.
	name, args := editorCommand(editorFields, tmpFile.Name())
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("editor exited with error: %v", err)
	}

	// Read back the edited content.
	edited, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		log.Fatalf("read temp file: %v", err)
	}
	newBody := string(edited)

	if strings.TrimSpace(newBody) == "" {
		fmt.Fprintln(os.Stderr, "Document is empty, skipping publish.")
		os.Exit(1)
	}

	if newBody == original {
		fmt.Fprintln(os.Stderr, "No changes, skipping publish.")
		return
	}

	if code := saveEdit(directDoc(client, host, path, token), result.Response, newBody); code != 0 {
		os.Exit(code)
	}
}

func graphMain(ctx context.Context, args []string) {
	// Handle "graph export" subcommand before flag parsing.
	if len(args) > 0 && args[0] == "export" {
		graphExportMain(args[1:])
		return
	}

	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	depth := fs.Int("depth", 2, "maximum crawl depth (link hops from start)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	noCache := fs.Bool("no-cache", false, "disable caching")
	cacheDir := fs.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus graph [-depth N] [-insecure] mark://host:port/path\n")
		fmt.Fprintf(os.Stderr, "       demarkus graph export [-o file.md]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}

	rawURL := fs.Arg(0)

	opts := fetch.Options{Insecure: *insecure}
	if !*noCache {
		opts.Cache = cache.New(*cacheDir)
	}
	client := fetch.NewClient(opts)
	defer client.Close()

	ts := tokens.LoadDefault()
	// A bad URL leaves the origin empty; the crawl reports the parse error itself.
	cred := tokens.Credential{}
	if target, parseErr := links.ParseMark(rawURL); parseErr == nil {
		cred.Origin = target.DialHost()
	}

	gs, err := graphstore.Load(graphstore.DefaultPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: graph store unavailable, results will not be persisted: %v\n", err)
	}

	fmt.Printf("Crawling %s (depth %d)...\n", rawURL, *depth)

	g, err := gs.CrawlAndPersist(ctx, rawURL, graphstore.NewFetchFunc(client, tokens.Resolver{Credential: cred, Store: ts}), graphstore.CrawlOptions{
		MaxDepth: *depth,
		OnNode: func(n *graph.Node) {
			title := n.Title
			if title == "" {
				title = n.URL
			}
			fmt.Printf("  [%s] %s (%d links)%s\n", n.Status, title, n.LinkCount, n.Observation.Annotation())
		},
	})
	if err != nil && g == nil {
		log.Fatal(err)
	}
	if warning := graph.CrawlWarning(err, g.Outcome); warning != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", warning)
	}
	fmt.Printf("\nGraph: %d nodes, %d edges\n%s\n", g.NodeCount(), g.EdgeCount(), g.Outcome.Summary())
	if g.EdgeCount() > 0 {
		fmt.Println("\nEdges:")
		for _, e := range g.GetEdges() {
			from := nodeLabel(g, e.From)
			to := nodeLabel(g, e.To)
			fmt.Printf("  %s -> %s\n", from, to)
		}
	}
}

func nodeLabel(g *graph.Graph, url string) string {
	if n := g.GetNode(url); n != nil && n.Title != "" {
		return n.Title
	}
	return url
}

func graphExportMain(args []string) {
	fs := flag.NewFlagSet("graph export", flag.ExitOnError)
	outFile := fs.String("o", "", "output file (default: stdout)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus graph export [-o file.md]\n\nExport the stored graph as a publishable markdown document.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}

	gs, err := graphstore.Load(graphstore.DefaultPath())
	if err != nil {
		log.Fatalf("failed to load graph store: %v", err)
	}

	md := gs.Export()

	if *outFile != "" {
		if err := os.WriteFile(*outFile, []byte(md), 0o644); err != nil {
			log.Fatalf("failed to write %s: %v", *outFile, err)
		}
		fmt.Fprintf(os.Stderr, "Exported graph (%d nodes, %d edges) to %s\n", gs.NodeCount(), gs.EdgeCount(), *outFile)
	} else {
		fmt.Print(md)
	}
}

func infoMain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus info [-insecure] mark://host:port\n\n")
		fmt.Fprintf(os.Stderr, "Fetch the agent manifest from a Mark Protocol server.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}

	host, err := links.DialHost(fs.Arg(0))
	if err != nil {
		log.Fatalf("invalid URL: %v", err)
	}

	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	result, err := client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: protocol.WellKnownManifestPath})
	if err != nil {
		log.Fatal(err)
	}

	if result.Response.Status == protocol.StatusNotFound {
		fmt.Fprintln(os.Stderr, "No agent manifest found at "+protocol.WellKnownManifestPath)
		os.Exit(1)
	}

	fmt.Printf("[%s]", result.Response.Status)
	for k, v := range result.Response.Metadata {
		fmt.Printf(" %s=%s", k, v)
	}
	fmt.Println()
	fmt.Print(result.Response.Body)
}

func tokenMain(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: demarkus token <add|remove|list>\n")
		fmt.Fprintf(os.Stderr, "  add    mark://host:port <token>  Store a token for a server\n")
		fmt.Fprintf(os.Stderr, "  remove mark://host:port          Remove a stored token\n")
		fmt.Fprintf(os.Stderr, "  list                             List servers with stored tokens\n")
		os.Exit(1)
	}

	ts, err := tokens.Load(tokens.DefaultPath())
	if err != nil {
		log.Fatalf("load tokens: %v", err)
	}

	switch args[0] {
	case "add":
		if len(args) < 3 {
			log.Fatal("usage: demarkus token add mark://host:port <token>")
		}
		host, err := links.DialHost(args[1])
		if err != nil {
			log.Fatalf("invalid URL: %v", err)
		}
		if err := ts.Set(host, args[2]); err != nil {
			log.Fatalf("save token: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Token stored for %s\n", host)

	case "remove":
		if len(args) < 2 {
			log.Fatal("usage: demarkus token remove mark://host:port")
		}
		host, err := links.DialHost(args[1])
		if err != nil {
			log.Fatalf("invalid URL: %v", err)
		}
		if err := ts.Remove(host); err != nil {
			log.Fatalf("remove token: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Token removed for %s\n", host)

	case "list":
		hosts := ts.Hosts()
		if len(hosts) == 0 {
			fmt.Println("No stored tokens.")
			return
		}
		for _, h := range hosts {
			fmt.Println(h)
		}

	default:
		log.Fatalf("unknown token command: %s", args[0])
	}
}

// joinMain stores the token carried by a join URL for the host, so
// subsequent requests resolve it automatically.
func joinMain(args []string) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: demarkus join 'mark://host[:port]#token=...'\n")
		fmt.Fprintf(os.Stderr, "Stores the join URL's token for the host; quote the URL so the shell keeps the fragment.\n")
		os.Exit(1)
	}
	j, err := joinurl.Parse(args[0])
	if err != nil {
		log.Fatal(err)
	}
	if j.Token == "" {
		log.Fatal("join URL carries no token; nothing to store (did the shell strip the #fragment? quote the URL)")
	}
	host, err := links.DialHost("mark://" + j.Host)
	if err != nil {
		log.Fatal(err)
	}
	ts, err := tokens.Load(tokens.DefaultPath())
	if err != nil {
		log.Fatalf("load tokens: %v", err)
	}
	if err := ts.Set(host, j.Token); err != nil {
		log.Fatalf("save token: %v", err)
	}
	fmt.Fprintf(os.Stderr, "Joined %s: token stored.\n", host)
	fmt.Fprintf(os.Stderr, "Try: demarkus mark://%s/index.md\n", host)
}

func bookmarkMain(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: demarkus bookmark <add|list|remove>\n")
		fmt.Fprintf(os.Stderr, "  add    [-insecure] mark://host:port/path  Bookmark a document\n")
		fmt.Fprintf(os.Stderr, "  remove mark://host:port/path              Remove a bookmark\n")
		fmt.Fprintf(os.Stderr, "  list                                      List all bookmarks\n")
		os.Exit(1)
	}

	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("bookmark add", flag.ExitOnError)
		insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
		_ = fs.Parse(args[1:]) // ExitOnError: Parse exits, never returns an error
		if fs.NArg() < 1 {
			log.Fatal("usage: demarkus bookmark add [-insecure] mark://host:port/path")
		}
		rawURL := fs.Arg(0)
		target, err := links.ParseMark(rawURL)
		if err != nil {
			log.Fatalf("invalid URL: %v", err)
		}
		host, path := target.DialHost(), target.Path

		// Fetch the document to extract the title.
		title := path
		client := fetch.NewClient(fetch.Options{Insecure: *insecure})
		defer client.Close()

		token := tokens.Resolve(tokens.Credential{Origin: host}, host, tokens.LoadDefault())
		result, err := client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: path, Token: token})
		if err == nil && result.Response.Status == protocol.StatusOK {
			if t := links.ExtractTitle(result.Response.Body); t != "" {
				title = t
			}
		}

		bs, err := bookmarks.Load(bookmarks.DefaultPath())
		if err != nil {
			log.Fatalf("load bookmarks: %v", err)
		}
		if bs.Has(rawURL) {
			fmt.Fprintln(os.Stderr, "Already bookmarked.")
			return
		}
		if err := bs.Add(rawURL, title); err != nil {
			log.Fatalf("add bookmark: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Bookmarked: [%s](%s)\n", title, rawURL)

	case "remove":
		if len(args) < 2 {
			log.Fatal("usage: demarkus bookmark remove mark://host:port/path")
		}
		rawURL := args[1]
		bs, err := bookmarks.Load(bookmarks.DefaultPath())
		if err != nil {
			log.Fatalf("load bookmarks: %v", err)
		}
		if err := bs.Remove(rawURL); err != nil {
			log.Fatalf("remove bookmark: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Bookmark removed: %s\n", rawURL)

	case "list":
		bs, err := bookmarks.Load(bookmarks.DefaultPath())
		if err != nil {
			log.Fatalf("load bookmarks: %v", err)
		}
		if len(bs.List()) == 0 {
			fmt.Println("No bookmarks.")
			return
		}
		fmt.Print(bs.Render())

	default:
		log.Fatalf("unknown bookmark command: %s", args[0])
	}
}

// resolveBody returns the request body from the flag or stdin for write verbs.
func resolveBody(verb, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if verb != protocol.VerbPublish && verb != protocol.VerbAppend {
		return ""
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		log.Fatalf("stat stdin: %v", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("read stdin: %v", err)
		}
		return string(data)
	}
	return ""
}

// editorCommand splits $EDITOR fields and appends the file path.
// Returns the executable name and its arguments.
// waitEditors maps GUI editor binaries that return immediately to the flag
// that makes them block until the file is closed.
var waitEditors = map[string]string{
	"zed":  "--wait",
	"code": "--wait",
	"subl": "--wait",
	"atom": "--wait",
}

func editorCommand(fields []string, file string) (name string, args []string) {
	args = make([]string, len(fields)-1, len(fields)+1)
	copy(args, fields[1:])

	base := filepath.Base(fields[0])
	if waitFlag, ok := waitEditors[base]; ok {
		hasWait := false
		for _, a := range args {
			if a == waitFlag || a == "-w" {
				hasWait = true
				break
			}
		}
		if !hasWait {
			args = append(args, waitFlag)
		}
	}

	args = append(args, file)
	return fields[0], args
}

func lookupMain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("lookup", flag.ExitOnError)
	query := fs.String("query", "", "subject to look up; matched against document tags and titles (required)")
	filter := fs.String("filter", "", "comma-separated key=value predicates (e.g. project=broker,modified-after=2025-01-01)")
	limit := fs.Int("limit", 0, "maximum results (0 = server default)")
	match := fs.String("match", "", "catalog (default) or body: match section text too; rows carry path#anchor and a snippet")
	authToken := fs.String("auth", "", "auth token (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	verbose := fs.Bool("v", false, "show status and metadata header before the results table")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus lookup -query SUBJECT [-filter K=V,...] [-limit N] [-match body] [-auth TOKEN] mark://host:port/scope/\n\n")
		fmt.Fprintf(os.Stderr, "Look up documents by subject against the server's catalog. Use / as the scope\n")
		fmt.Fprintf(os.Stderr, "to search everything, or a subtree like /docs/ to narrow it. -match body also\n")
		fmt.Fprintf(os.Stderr, "matches section text; a server without body match answers from the catalog.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	if *query == "" {
		log.Fatal("lookup requires -query")
	}

	target := mustTarget(fs.Arg(0))
	host, scope := target.DialHost(), target.Path

	token := tokens.Resolve(tokens.Credential{Explicit: *authToken, Origin: host}, host, tokens.LoadDefault())

	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	req := fetch.LookupRequest{
		Host: host, Scope: scope, Token: token,
		Query: *query, Filter: *filter, Limit: *limit, Match: *match,
	}
	result, err := client.Lookup(ctx, req)
	if err != nil {
		log.Fatal(err)
	}
	if fetch.AnsweredFromCatalog(req, result) {
		fmt.Fprintln(os.Stderr, "note: "+fetch.CatalogFallbackNote)
	}

	printResult(result, *verbose)
}

// exitCodeForStatus lets scripts detect a refused request: the body of a
// conflict or not-found is still printed, but the exit is non zero.
func exitCodeForStatus(status string) int {
	switch status {
	case protocol.StatusOK, protocol.StatusCreated, protocol.StatusNotModified:
		return 0
	}
	return 1
}

// printResult writes the body, the status line when verbose, and exits non
// zero for a refused request.
func printResult(result fetch.Result, verbose bool) {
	if verbose {
		line := statusLine(result.Response.Status, result.Response.Metadata)
		if result.FromCache {
			line += " (cached)"
		}
		fmt.Fprintln(os.Stderr, line)
	}
	fmt.Print(result.Response.Body)
	exitOnFailedStatus(result.Response.Status)
}

func exitOnFailedStatus(status string) {
	if code := exitCodeForStatus(status); code != 0 {
		fmt.Fprintf(os.Stderr, "demarkus: %s\n", status)
		os.Exit(code)
	}
}

// metaFlag collects repeatable `-meta key=value` publisher-metadata pairs.
// The underlying map is shared by flag.Var, so Set accumulates across repeats.
type metaFlag map[string]string

func (m metaFlag) String() string {
	if len(m) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, k+"="+v)
	}
	return strings.Join(pairs, ",")
}

func (m metaFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("invalid -meta %q: want key=value", s)
	}
	if !protocol.IsValidMetaKey(k) {
		return fmt.Errorf("invalid -meta key %q: lowercase letters, digits, and hyphens only", k)
	}
	m[k] = v
	return nil
}

// metaMap returns the collected publisher metadata, or nil when empty so the
// client omits the frontmatter entirely rather than sending an empty block.
func metaMap(m metaFlag) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// confirmRetention guards the destructive retention metadata key: the write
// carrying it (and every later one) permanently deletes versions older than
// the newest N. Interactive runs confirm on the prompt; non-interactive runs
// (stdin is not a TTY) must pass -yes so scripts state the intent explicitly.
func confirmRetention(meta map[string]string, yes bool, in *os.File, out io.Writer) error {
	r, ok := meta["retention"]
	if !ok || yes {
		return nil
	}
	// Only a positive integer can prune; anything else (0, negative,
	// non-numeric) is rejected by the server's validation, so a destructive
	// warning here would be misleading — let the server report the precise
	// bad-request error instead. store.ParseRetention is the same predicate
	// the server enforces.
	if _, ok := storefmt.ParseRetention(r); !ok {
		return nil
	}
	if !term.IsTerminal(int(in.Fd())) {
		return fmt.Errorf("retention=%s permanently deletes older versions on this and every later write carrying it; pass -yes to confirm in non-interactive mode", r)
	}
	if _, err := fmt.Fprintf(out, "retention=%s permanently deletes all but the newest %s versions of this document, on this write and every later write carrying the key. Continue? [y/N] ", r, r); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("aborted: retention not confirmed")
	}
}

func validateVerb(verb string) error {
	if !protocol.IsValidVerb(verb) {
		return fmt.Errorf("unsupported verb: %s (valid: FETCH, LIST, VERSIONS, PUBLISH, ARCHIVE, APPEND)", verb)
	}
	return nil
}

// mustTarget parses the URL argument or exits.
func mustTarget(raw string) links.Target {
	target, err := links.ParseMark(raw)
	if err != nil {
		log.Fatalf("invalid URL: %v", err)
	}
	return target
}
