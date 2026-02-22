package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/latebit/demarkus/protocol"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "graph" {
		graphMain(os.Args[2:])
		return
	}
	requestMain()
}

func requestMain() {
	verb := flag.String("X", protocol.VerbFetch, "request verb (FETCH, LIST, VERSIONS, PUBLISH)")
	body := flag.String("body", "", "request body (for PUBLISH); reads stdin if omitted")
	authToken := flag.String("auth", "", "auth token for PUBLISH requests (env: DEMARKUS_AUTH)")
	noCache := flag.Bool("no-cache", false, "disable caching")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	cacheDir := flag.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus [-X VERB] [-body TEXT] [-auth TOKEN] mark://host:port/path\n")
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

	// Auth token: flag takes precedence over env var.
	token := *authToken
	if token == "" {
		token = os.Getenv("DEMARKUS_AUTH")
	}

	// For PUBLISH: read body from -body flag or stdin.
	reqBody := *body
	if *verb == protocol.VerbPublish && reqBody == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("read stdin: %v", err)
		}
		reqBody = string(data)
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

var validVerbs = map[string]bool{
	protocol.VerbFetch:    true,
	protocol.VerbList:     true,
	protocol.VerbVersions: true,
	protocol.VerbPublish:  true,
}

func validateVerb(verb string) error {
	if !validVerbs[verb] {
		return fmt.Errorf("unsupported verb: %s (valid: FETCH, LIST, VERSIONS, PUBLISH)", verb)
	}
	return nil
}
