package graph

import (
	"context"
	"strings"
	"sync"
	"unicode"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
)

// Fetcher abstracts the ability to fetch a document by host and path.
type Fetcher interface {
	Fetch(host, path string) (FetchResult, error)
}

// FetchResult holds the response from a fetch operation.
type FetchResult struct {
	Status   string
	Body     string
	Metadata map[string]string // publisher metadata from the response, may be nil
}

// CrawlOptions configures the graph crawler.
type CrawlOptions struct {
	MaxDepth int         // maximum link hops from start (default: 2, 0 = start node only, -1 = use default)
	Workers  int         // concurrent fetch goroutines (default: 5)
	OnNode   func(*Node) // called when a node is discovered, may be nil
}

func (o *CrawlOptions) applyDefaults() {
	if o.MaxDepth < 0 {
		o.MaxDepth = 2
	}
	if o.Workers <= 0 {
		o.Workers = 5
	}
}

type crawlItem struct {
	url   string
	depth int
}

// RelRef is a typed-relation reference parsed from rel-<predicate> metadata.
type RelRef struct {
	Rel    string // predicate, e.g. "supersedes"
	Target string // resolved target URL
}

// RelEdges parses rel-<predicate> publisher-metadata keys into typed-relation
// references. Values are comma-separated refs resolved against docURL.
// Malformed refs (empty, internal whitespace) and self-references are skipped
// silently: bad metadata must never fail a crawl.
func RelEdges(docURL string, metadata map[string]string) []RelRef {
	var refs []RelRef
	for key, val := range metadata {
		pred, ok := strings.CutPrefix(key, "rel-")
		if !ok || pred == "" {
			continue
		}
		for ref := range strings.SplitSeq(val, ",") {
			ref = strings.TrimSpace(ref)
			if ref == "" || strings.IndexFunc(ref, unicode.IsSpace) >= 0 {
				continue
			}
			resolved := links.Resolve(docURL, ref)
			if resolved == docURL {
				continue
			}
			refs = append(refs, RelRef{Rel: pred, Target: resolved})
		}
	}
	return refs
}

// Crawl performs a BFS crawl starting from startURL, following mark:// links
// up to opts.MaxDepth hops. It uses fetcher to retrieve documents and builds
// a Graph of all discovered nodes and edges.
//
// ParseURL is used to split mark:// URLs into host and path for fetching.
// Links to non-mark schemes are recorded as nodes but not crawled.
func Crawl(ctx context.Context, startURL string, fetcher Fetcher, parseURL func(string) (string, string, error), opts CrawlOptions) (*Graph, error) {
	opts.applyDefaults()
	g := New()

	queue := make(chan crawlItem, 1000)
	var wg sync.WaitGroup

	// Track visited URLs to avoid duplicates.
	visited := make(map[string]bool)
	var visitMu sync.Mutex

	// markVisited returns true if the URL was not yet visited, and marks it.
	markVisited := func(url string) bool {
		visitMu.Lock()
		defer visitMu.Unlock()
		if visited[url] {
			return false
		}
		visited[url] = true
		return true
	}

	// Seed the queue.
	if !markVisited(startURL) {
		return g, nil
	}
	wg.Add(1)
	queue <- crawlItem{url: startURL, depth: 0}

	// Start workers.
	for range opts.Workers {
		go func() {
			for item := range queue {
				func() {
					defer wg.Done()

					if ctx.Err() != nil {
						return
					}

					node := &Node{
						URL:   item.url,
						Depth: item.depth,
					}

					// Only crawl mark:// URLs.
					if !strings.HasPrefix(item.url, "mark://") {
						node.Status = "external"
						g.AddNode(node)
						if opts.OnNode != nil {
							opts.OnNode(node)
						}
						return
					}

					host, path, err := parseURL(item.url)
					if err != nil {
						node.Status = "error"
						g.AddNode(node)
						if opts.OnNode != nil {
							opts.OnNode(node)
						}
						return
					}

					result, err := fetcher.Fetch(host, path)
					if err != nil {
						node.Status = "error"
						g.AddNode(node)
						if opts.OnNode != nil {
							opts.OnNode(node)
						}
						return
					}

					node.Status = result.Status
					if result.Status == protocol.StatusOK {
						node.Title = links.ExtractTitle(result.Body)
						anchored := mdoutline.AnchoredLinks(result.Body)
						node.LinkCount = len(anchored) // body links only; rel- edges do not count

						enqueue := func(resolved string) {
							if item.depth < opts.MaxDepth && markVisited(resolved) {
								wg.Add(1)
								child := crawlItem{url: resolved, depth: item.depth + 1}
								go func() { queue <- child }()
							}
						}

						for _, l := range anchored {
							resolved := links.Resolve(item.url, l.Dest)
							g.AddEdgeInfo(Edge{From: item.url, To: resolved, Label: l.Label, Anchor: l.Anchor, Count: 1})
							enqueue(resolved)
						}

						for _, r := range RelEdges(item.url, result.Metadata) {
							g.AddEdgeInfo(Edge{From: item.url, To: r.Target, Rel: r.Rel, Count: 1})
							enqueue(r.Target)
						}
					}

					g.AddNode(node)
					if opts.OnNode != nil {
						opts.OnNode(node)
					}
				}()
			}
		}()
	}

	wg.Wait()
	close(queue)

	return g, nil
}
