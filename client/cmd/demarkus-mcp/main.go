// Command demarkus-mcp is an MCP server that exposes the Mark Protocol as tools
// for LLM agents. It supports fetching documents, listing directories, and
// crawling link graphs via stdio transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	defaultHost := flag.String("host", "", "default Mark server (e.g. mark://localhost:6309)")
	token := flag.String("token", "", "auth token for capability-based authentication")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	noCache := flag.Bool("no-cache", false, "disable response caching")
	cacheDir := flag.String("cache-dir", cache.DefaultDir(), "cache directory")
	flag.Parse()

	opts := fetch.Options{Insecure: *insecure}
	if !*noCache {
		opts.Cache = cache.New(*cacheDir)
	}
	client := fetch.NewClient(opts)
	defer client.Close()

	s := server.NewMCPServer("demarkus-mcp", "0.1.0")

	h := &handler{client: client, defaultHost: *defaultHost, token: *token}
	s.AddTool(markFetchTool(*defaultHost), h.markFetch)
	s.AddTool(markListTool(*defaultHost), h.markList)
	s.AddTool(markGraphTool(*defaultHost), h.markGraph)
	s.AddTool(markPublishTool(*defaultHost), h.markPublish)

	if err := server.ServeStdio(s); err != nil {
		log.Fatal(err)
	}
}

type handler struct {
	client      *fetch.Client
	defaultHost string
	token       string
}

// resolveURL parses a mark:// URL or bare path (when -host is set) into host and path.
func (h *handler) resolveURL(rawURL string) (host, path string, err error) {
	if strings.HasPrefix(rawURL, "/") {
		if h.defaultHost == "" {
			return "", "", fmt.Errorf("bare path %q requires -host flag", rawURL)
		}
		return fetch.ParseMarkURL(h.defaultHost + rawURL)
	}
	return fetch.ParseMarkURL(rawURL)
}

// Tool definitions.

// urlHint returns a description suffix telling the LLM how to format URLs.
// When a default host is configured, it tells the LLM to use bare paths.
// Otherwise, it tells the LLM to use full mark:// URLs.
func urlHint(host string) string {
	if host != "" {
		return fmt.Sprintf("Connected to %s. Use bare paths like /index.md.", host)
	}
	return "Use full mark:// URLs, e.g. mark://host/index.md."
}

func urlDesc(host string) string {
	if host != "" {
		return "bare path, e.g. /index.md or /docs/"
	}
	return "mark:// URL, e.g. mark://host/index.md"
}

func markFetchTool(host string) mcp.Tool {
	return mcp.NewTool("mark_fetch",
		mcp.WithDescription(
			"Fetch a document from a Mark Protocol server. "+
				"Returns the document status and markdown body. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
	)
}

func markListTool(host string) mcp.Tool {
	return mcp.NewTool("mark_list",
		mcp.WithDescription(
			"List documents and subdirectories on a Mark Protocol server. "+
				"Use this to discover what documents exist. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
	)
}

func markGraphTool(host string) mcp.Tool {
	return mcp.NewTool("mark_graph",
		mcp.WithDescription(
			"Crawl outbound links from a document and return the link graph. "+
				"Follows mark:// links up to the specified depth. External links are "+
				"recorded but not followed. Use this to understand document relationships "+
				"or find broken links. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
		mcp.WithNumber("depth",
			mcp.Description("Maximum link depth to follow (default 2, max 5)"),
		),
	)
}

func markPublishTool(host string) mcp.Tool {
	return mcp.NewTool("mark_publish",
		mcp.WithDescription(
			"Publish or update a document on a Mark Protocol server. "+
				"Requires an auth token configured via the -token flag. "+
				"The body should be valid markdown content. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("markdown content to publish"),
		),
	)
}

// Tool handlers.
// Handler signatures are dictated by mcp-go's ToolHandlerFunc type.

func (h *handler) markFetch(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	result, err := h.client.Fetch(host, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch failed: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("status: %s\n\n%s", result.Response.Status, result.Response.Body)), nil
}

func (h *handler) markList(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	result, err := h.client.List(host, path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list failed: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("status: %s\n\n%s", result.Response.Status, result.Response.Body)), nil
}

func (h *handler) markPublish(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	if h.token == "" {
		return mcp.NewToolResultError("publish requires -token flag"), nil
	}

	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	body, err := req.RequireString("body")
	if err != nil {
		return mcp.NewToolResultError("body is required"), nil
	}

	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	result, err := h.client.Publish(host, path, body, h.token)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("status: %s", result.Response.Status)), nil
}

func (h *handler) markGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	depth := max(1, min(req.GetInt("depth", 2), 5))

	if _, _, err := h.resolveURL(rawURL); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	startURL := rawURL
	if strings.HasPrefix(rawURL, "/") {
		startURL = h.defaultHost + rawURL
	}

	fetcher := &graph.ClientFetcher{
		FetchFunc: func(host, path string) (string, string, error) {
			r, fetchErr := h.client.Fetch(host, path)
			if fetchErr != nil {
				return "", "", fetchErr
			}
			return r.Response.Status, r.Response.Body, nil
		},
	}

	const maxNodes = 200
	var nodeCount int
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, err := graph.Crawl(ctx, startURL, fetcher, fetch.ParseMarkURL, graph.CrawlOptions{
		MaxDepth: depth,
		Workers:  5,
		OnNode: func(_ *graph.Node) {
			nodeCount++
			if nodeCount >= maxNodes {
				cancel()
			}
		},
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("crawl failed: %v", err)), nil
	}

	return mcp.NewToolResultText(formatGraph(g, startURL)), nil
}

// formatGraph renders a graph as a plain-text summary for LLM consumption.
func formatGraph(g *graph.Graph, startURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Crawled %d nodes, %d edges from %s\n", g.NodeCount(), g.EdgeCount(), startURL)

	nodes := g.AllNodes()
	if len(nodes) == 0 {
		return b.String()
	}

	b.WriteString("\nNodes:\n")
	for _, n := range nodes {
		title := n.Title
		if title == "" {
			title = "(no title)"
		}
		fmt.Fprintf(&b, "  [%-9s] %-40s %q  %d links\n", n.Status, n.URL, title, n.LinkCount)
	}

	edges := g.GetEdges()
	if len(edges) > 0 {
		b.WriteString("\nEdges:\n")
		for _, e := range edges {
			fmt.Fprintf(&b, "  %s -> %s\n", e.From, e.To)
		}
	}

	return b.String()
}
