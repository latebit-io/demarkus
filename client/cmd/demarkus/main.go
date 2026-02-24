package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/latebit/demarkus/client/internal/tokens"
	"github.com/latebit/demarkus/protocol"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "edit":
			editMain(os.Args[2:])
			return
		case "graph":
			graphMain(os.Args[2:])
			return
		case "token":
			tokenMain(os.Args[2:])
			return
		}
	}
	requestMain()
}

func requestMain() {
	verb := flag.String("X", protocol.VerbFetch, "request verb (FETCH, LIST, VERSIONS, PUBLISH, ARCHIVE)")
	body := flag.String("body", "", "request body (for PUBLISH); reads stdin if omitted")
	authToken := flag.String("auth", "", "auth token for PUBLISH/ARCHIVE requests (env: DEMARKUS_AUTH)")
	noCache := flag.Bool("no-cache", false, "disable caching")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	cacheDir := flag.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus [-X VERB] [-body TEXT] [-auth TOKEN] mark://host:port/path\n")
		fmt.Fprintf(os.Stderr, "       demarkus edit [-auth TOKEN] [-insecure] mark://host:port/path.md\n")
		fmt.Fprintf(os.Stderr, "       demarkus graph [-depth N] [-insecure] mark://host:port/path\n\n")
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

	host, path, err := fetch.ParseMarkURL(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	opts := fetch.Options{Insecure: *insecure}
	if !*noCache {
		opts.Cache = cache.New(*cacheDir)
	}

	// Auth token: flag > env var > stored token for host.
	token := *authToken
	if token == "" {
		token = os.Getenv("DEMARKUS_AUTH")
	}
	if token == "" {
		if ts, err := tokens.Load(tokens.DefaultPath()); err == nil {
			token = ts.Get(host)
		}
	}

	// For PUBLISH: read body from -body flag or stdin (if piped).
	// When stdin is a terminal and no -body is given, send an empty body
	// (used for unarchiving).
	reqBody := *body
	if *verb == protocol.VerbPublish && reqBody == "" {
		info, err := os.Stdin.Stat()
		if err != nil {
			log.Fatalf("stat stdin: %v", err)
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				log.Fatalf("read stdin: %v", err)
			}
			reqBody = string(data)
		}
	}

	client := fetch.NewClient(opts)
	defer client.Close()

	var result fetch.Result
	switch *verb {
	case protocol.VerbFetch:
		result, err = client.Fetch(host, path)
	case protocol.VerbList:
		result, err = client.List(host, path)
	case protocol.VerbVersions:
		result, err = client.Versions(host, path)
	case protocol.VerbPublish:
		result, err = client.Publish(host, path, reqBody, token)
	case protocol.VerbArchive:
		result, err = client.Archive(host, path, token)
	}
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("[%s]", result.Response.Status)
	for k, v := range result.Response.Metadata {
		fmt.Printf(" %s=%s", k, v)
	}
	if result.FromCache {
		fmt.Printf(" (cached)")
	}
	fmt.Println()
	fmt.Print(result.Response.Body)
}

func editMain(args []string) {
	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	authToken := fs.String("auth", "", "auth token (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	cacheDir := fs.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus edit [-auth TOKEN] [-insecure] mark://host:port/path.md\n\n")
		fmt.Fprintf(os.Stderr, "Fetch a document, open it in $EDITOR, and publish changes.\n")
		fmt.Fprintf(os.Stderr, "Creates a new document if it doesn't exist.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}

	host, path, err := fetch.ParseMarkURL(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	// Resolve auth token: flag > env > stored token.
	token := *authToken
	if token == "" {
		token = os.Getenv("DEMARKUS_AUTH")
	}
	if token == "" {
		if ts, err := tokens.Load(tokens.DefaultPath()); err == nil {
			token = ts.Get(host)
		}
	}

	client := fetch.NewClient(fetch.Options{
		Insecure: *insecure,
		Cache:    cache.New(*cacheDir),
	})
	defer client.Close()

	// Fetch the current document content.
	var original string
	result, err := client.Fetch(host, path)
	if err != nil {
		log.Fatal(err)
	}
	switch result.Response.Status {
	case protocol.StatusOK:
		original = result.Response.Body
	case protocol.StatusNotFound:
		// New document — start with empty content.
		fmt.Fprintf(os.Stderr, "Document not found, creating new document.\n")
	default:
		log.Fatalf("fetch failed: %s", result.Response.Status)
	}

	// Write content to a temp file for editing.
	tmpFile, err := os.CreateTemp("", "demarkus-edit-*.md")
	if err != nil {
		log.Fatalf("create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.WriteString(original); err != nil {
		log.Fatalf("write temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		log.Fatalf("close temp file: %v", err)
	}

	// Open the editor.
	cmd := exec.Command(editor, tmpFile.Name())
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

	// Publish the edited content.
	result, err = client.Publish(host, path, newBody, token)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("[%s]", result.Response.Status)
	for k, v := range result.Response.Metadata {
		fmt.Printf(" %s=%s", k, v)
	}
	fmt.Println()
	if result.Response.Body != "" {
		fmt.Print(result.Response.Body)
	}
}

func graphMain(args []string) {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	depth := fs.Int("depth", 2, "maximum crawl depth (link hops from start)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	noCache := fs.Bool("no-cache", false, "disable caching")
	cacheDir := fs.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus graph [-depth N] [-insecure] mark://host:port/path\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

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

	fetcher := &graph.ClientFetcher{
		FetchFunc: func(host, path string) (string, string, error) {
			r, err := client.Fetch(host, path)
			if err != nil {
				return "", "", err
			}
			return r.Response.Status, r.Response.Body, nil
		},
	}

	fmt.Printf("Crawling %s (depth %d)...\n", rawURL, *depth)

	g, err := graph.Crawl(context.Background(), rawURL, fetcher, fetch.ParseMarkURL, graph.CrawlOptions{
		MaxDepth: *depth,
		OnNode: func(n *graph.Node) {
			title := n.Title
			if title == "" {
				title = n.URL
			}
			fmt.Printf("  [%s] %s (%d links)\n", n.Status, title, n.LinkCount)
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\nGraph: %d nodes, %d edges\n", g.NodeCount(), g.EdgeCount())
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

func tokenMain(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: demarkus token <add|remove|list>\n")
		fmt.Fprintf(os.Stderr, "  add    mark://host:port <token>  Store a token for a server\n")
		fmt.Fprintf(os.Stderr, "  remove mark://host:port          Remove a stored token\n")
		fmt.Fprintf(os.Stderr, "  list                             List servers with stored tokens\n")
		os.Exit(1)
	}

	switch args[0] {
	case "add":
		if len(args) < 3 {
			log.Fatal("usage: demarkus token add mark://host:port <token>")
		}
		host, _, err := fetch.ParseMarkURL(args[1])
		if err != nil {
			log.Fatalf("invalid URL: %v", err)
		}
		ts, err := tokens.Load(tokens.DefaultPath())
		if err != nil {
			log.Fatalf("load tokens: %v", err)
		}
		if err := ts.Set(host, args[2]); err != nil {
			log.Fatalf("save token: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Token stored for %s\n", host)

	case "remove":
		if len(args) < 2 {
			log.Fatal("usage: demarkus token remove mark://host:port")
		}
		host, _, err := fetch.ParseMarkURL(args[1])
		if err != nil {
			log.Fatalf("invalid URL: %v", err)
		}
		ts, err := tokens.Load(tokens.DefaultPath())
		if err != nil {
			log.Fatalf("load tokens: %v", err)
		}
		if err := ts.Remove(host); err != nil {
			log.Fatalf("remove token: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Token removed for %s\n", host)

	case "list":
		ts, err := tokens.Load(tokens.DefaultPath())
		if err != nil {
			log.Fatalf("load tokens: %v", err)
		}
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

var validVerbs = map[string]bool{
	protocol.VerbFetch:    true,
	protocol.VerbList:     true,
	protocol.VerbVersions: true,
	protocol.VerbPublish:  true,
	protocol.VerbArchive:  true,
}

func validateVerb(verb string) error {
	if !validVerbs[verb] {
		return fmt.Errorf("unsupported verb: %s (valid: FETCH, LIST, VERSIONS, PUBLISH, ARCHIVE)", verb)
	}
	return nil
}
