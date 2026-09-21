package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
)

// Fetcher fetches one parsed mark:// target. A direct client dials
// target.DialHost(); the broker routes target.Hostname() as a world name.
type Fetcher interface {
	Fetch(ctx context.Context, target links.Target) (FetchResult, error)
}

// FetchResult holds the response from a fetch operation.
type FetchResult struct {
	Source   string // logical source URL when graph identity is a routing alias
	Status   string
	Body     string
	Metadata map[string]string // publisher metadata from the response, may be nil
}

// CrawlOptions configures the graph crawler.
type CrawlOptions struct {
	MaxDepth       int         // maximum link hops from start (default: 2, 0 = start node only, -1 = use default)
	Workers        int         // concurrent fetch goroutines (default: 5)
	MaxNodes       int         // admitted nodes (default: 1000)
	MaxFrontier    int         // queued nodes across both BFS levels (default: MaxNodes)
	MaxFetchBytes  int64       // network reads and decoded payloads (default: 64 MiB each)
	MaxOutputBytes int         // summary bytes (default: 1 MiB, minimum: 1024)
	OnNode         func(*Node) // called when a node is discovered, may be nil
}

func (o *CrawlOptions) applyDefaults() {
	if o.MaxDepth < 0 {
		o.MaxDepth = 2
	}
	if o.Workers <= 0 {
		o.Workers = 5
	}
	o.Workers = min(o.Workers, 32)
	if o.MaxNodes <= 0 {
		o.MaxNodes = 1000
	}
	if o.MaxFrontier <= 0 {
		o.MaxFrontier = o.MaxNodes
	}
	o.MaxFrontier = min(o.MaxFrontier, o.MaxNodes)
	if o.MaxFetchBytes <= 0 {
		o.MaxFetchBytes = 64 << 20
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = 1 << 20
	}
	o.MaxOutputBytes = max(o.MaxOutputBytes, 1024)
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

// ExtractedEdges contains normalized edges from one fetched document.
type ExtractedEdges struct {
	BodyLinkCount int
	Edges         []Edge
	RejectedRels  []RejectedRel // rel- values that produced no edge
}

// ExtractDocumentEdges resolves body links and typed relations against docURL.
func ExtractDocumentEdges(docURL, body string, metadata map[string]string) ExtractedEdges {
	docURL = links.CanonicalURL(docURL)
	anchored := mdoutline.AnchoredLinks(body)
	extracted := ExtractedEdges{
		BodyLinkCount: len(anchored),
		Edges:         make([]Edge, 0, len(anchored)),
	}
	for _, link := range anchored {
		extracted.Edges = append(extracted.Edges, Edge{
			From:   docURL,
			To:     links.CanonicalURL(links.Resolve(docURL, link.Dest)),
			Label:  link.Label,
			Anchor: link.Anchor,
			Count:  1,
		})
	}
	relations := RelEdges(docURL, metadata)
	extracted.RejectedRels = relations.Rejected
	for _, relation := range relations.Refs {
		extracted.Edges = append(extracted.Edges, Edge{
			From:  docURL,
			To:    relation.Target,
			Rel:   relation.Rel,
			Count: 1,
		})
	}
	return extracted
}

// RejectedRel is one rel-<predicate> value that produced no edge, and why.
type RejectedRel struct {
	Key    string
	Value  string
	Reason string
}

// RelResult is what RelEdges read from one document's metadata.
type RelResult struct {
	Refs     []RelRef
	Rejected []RejectedRel
}

// RelEdges resolves comma-separated rel-<predicate> refs against docURL.
// Bad values never fail a crawl (ADR 0004); they come back as Rejected.
func RelEdges(docURL string, metadata map[string]string) RelResult {
	// Callers may pass a dial address (fedcrawl does); compare like with like
	// so a self-reference is not mistaken for an edge to a different node.
	docURL = links.CanonicalURL(docURL)
	var result RelResult
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pred, ok := strings.CutPrefix(key, "rel-")
		if !ok {
			continue
		}
		for ref := range strings.SplitSeq(metadata[key], ",") {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue // a trailing comma, not a reference
			}
			reason := ""
			resolved := ""
			switch {
			case pred == "":
				reason = "empty predicate"
			case strings.IndexFunc(ref, unicode.IsSpace) >= 0:
				reason = "whitespace in reference"
			default:
				resolved = links.CanonicalURL(links.Resolve(docURL, ref))
				if resolved == docURL {
					reason = "self reference"
				}
			}
			if reason != "" {
				result.Rejected = append(result.Rejected, RejectedRel{Key: key, Value: ref, Reason: reason})
				continue
			}
			result.Refs = append(result.Refs, RelRef{Rel: pred, Target: resolved})
		}
	}
	return result
}

// Crawl uses level barriers so first admission always has shortest-path depth.
// Partial crawls return valid observations and an ErrIncomplete outcome.
// Boundary edges are retained; their destinations are outside requested scope.
func Crawl(ctx context.Context, startURL string, fetcher Fetcher, opts CrawlOptions) (*Graph, error) {
	opts.applyDefaults()
	g := New()
	startURL = links.CanonicalURL(startURL)
	if len(startURL) > opts.MaxOutputBytes-512 || len(nodeSummary(&Node{URL: startURL, Incomplete: true})) > opts.MaxOutputBytes-512-len(startURL) {
		return nil, errors.New("crawl start URL exceeds output budget")
	}
	if strings.HasPrefix(startURL, "mark://") && fetcher == nil {
		return nil, fmt.Errorf("crawl %s: fetcher is required", startURL)
	}
	o := &CrawlOutcome{StartURL: startURL, MaxDepth: opts.MaxDepth}
	ctx, budget := fetch.WithResponseBudget(ctx, opts.MaxFetchBytes)
	run := crawlRun{graph: g, outcome: o, opts: opts, outputBytes: 512 + len(startURL), visited: make(map[string]bool)}
	var frontier []crawlItem
	if ctx.Err() == nil {
		frontier = []crawlItem{{url: startURL}}
		run.visited[startURL] = true
		o.Admitted = 1
		o.PeakFrontier = 1
	}
	for len(frontier) > 0 && ctx.Err() == nil {
		run.next = nil
		for len(frontier) > 0 && ctx.Err() == nil {
			batchSize := min(len(frontier), opts.Workers)
			batch := frontier[:batchSize]
			frontier = frontier[batchSize:]
			run.pending = len(frontier)
			results := make([]crawlFetch, batchSize)
			var wg sync.WaitGroup
			for i, item := range batch {
				wg.Go(func() { results[i] = fetchItem(ctx, item.url, fetcher) })
			}
			o.PeakWorkers = max(o.PeakWorkers, batchSize)
			wg.Wait()
			for i, item := range batch {
				run.observe(ctx, item, &results[i])
			}
			if budgetExhausted(o.Reasons) {
				frontier = nil
				run.next = nil
			}
		}
		frontier = run.next
	}
	if ctx.Err() != nil {
		o.addReason(ReasonCancelled)
		o.cause = ctx.Err()
	}
	o.ReadBytes = budget.BytesRead()
	o.Complete = len(o.Reasons) == 0
	g.Outcome = o
	if !o.Complete {
		return g, o
	}
	return g, nil
}

type crawlRun struct {
	graph       *Graph
	outcome     *CrawlOutcome
	opts        CrawlOptions
	visited     map[string]bool
	next        []crawlItem
	pending     int
	outputBytes int
}

func (r *crawlRun) observe(ctx context.Context, item crawlItem, res *crawlFetch) {
	if res.fetched {
		r.outcome.Fetches++
	}
	node := &Node{URL: item.url, Depth: item.depth, Status: res.result.Status}
	source := res.result.Source
	if source == "" {
		source = item.url
	}
	node.Observation = Observe(source, res.result.Metadata)
	r.classify(ctx, node, res)
	size := int64(len(res.result.Body) + len(res.result.Status))
	for key, value := range res.result.Metadata {
		size += int64(len(key) + len(value))
	}
	if size > r.opts.MaxFetchBytes-r.outcome.FetchedBytes {
		node.Incomplete = true
		r.outcome.addReason(ReasonByteCap)
	} else {
		r.outcome.FetchedBytes += size
	}
	var extracted ExtractedEdges
	if node.Status == protocol.StatusOK && !node.Incomplete {
		extracted = ExtractDocumentEdges(item.url, res.result.Body, res.result.Metadata)
		node.Title = links.ExtractTitle(res.result.Body)
		node.LinkCount = extracted.BodyLinkCount
		r.outcome.RejectedRels += len(extracted.RejectedRels)
	}
	node.Observation.Complete = SourceComplete(node)
	if !r.reserveOutput(len(nodeSummary(node)) + 32) {
		node = &Node{URL: item.url, Depth: item.depth, Incomplete: true}
		if !r.reserveOutput(len(nodeSummary(node))) {
			return
		}
	}
	for i := range extracted.Edges {
		edge := &extracted.Edges[i]
		// Repeated occurrences can grow the aggregated count's decimal width.
		if !r.reserveOutput(len(edgeSummary(edge)) + 20) {
			node.Incomplete = true
			break
		}
		r.graph.AddEdgeInfo(*edge)
		r.admit(edge.To, item.depth)
	}
	node.Observation.Complete = SourceComplete(node)
	if !node.Observation.Complete {
		node.Observation.Problem = "incomplete"
	}
	r.graph.AddNode(node)
	if r.opts.OnNode != nil {
		r.opts.OnNode(node)
	}
}

func (r *crawlRun) classify(ctx context.Context, node *Node, res *crawlFetch) {
	err := res.err
	if err != nil {
		node.Status = "error"
		node.Error = err.Error()
		if len(node.Error) > 256 {
			node.Error = node.Error[:256] + "..."
		}
		if errors.Is(err, fetch.ErrResponseBudget) {
			r.outcome.addReason(ReasonByteCap)
			return
		}
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return
		}
	} else if observedStatus(node.Status) || (!res.fetched && node.Status == "external") {
		return
	}
	r.outcome.addReason(ReasonFetchFailure)
	r.outcome.Failures++
}

func (r *crawlRun) reserveOutput(bytes int) bool {
	if bytes > r.opts.MaxOutputBytes-r.outputBytes {
		r.outcome.addReason(ReasonOutputCap)
		return false
	}
	r.outputBytes += bytes
	return true
}

func (r *crawlRun) admit(url string, parentDepth int) {
	if parentDepth == r.opts.MaxDepth || r.visited[url] {
		return
	}
	if r.outcome.Admitted == r.opts.MaxNodes {
		r.outcome.addReason(ReasonNodeCap)
		return
	}
	if r.pending+len(r.next) == r.opts.MaxFrontier {
		r.outcome.addReason(ReasonFrontierCap)
		return
	}
	r.visited[url] = true
	r.next = append(r.next, crawlItem{url: url, depth: parentDepth + 1})
	r.outcome.Admitted++
	r.outcome.PeakFrontier = max(r.outcome.PeakFrontier, r.pending+len(r.next))
}

func budgetExhausted(reasons []string) bool {
	for _, reason := range reasons {
		if reason == ReasonByteCap || reason == ReasonOutputCap {
			return true
		}
	}
	return false
}

type crawlFetch struct {
	result  FetchResult
	err     error
	fetched bool
}

func fetchItem(ctx context.Context, url string, fetcher Fetcher) crawlFetch {
	if err := ctx.Err(); err != nil {
		return crawlFetch{err: err}
	}
	if !strings.HasPrefix(url, "mark://") {
		return crawlFetch{result: FetchResult{Status: "external"}}
	}
	target, err := links.ParseMark(url)
	if err != nil {
		return crawlFetch{err: err}
	}
	result, err := fetcher.Fetch(ctx, target)
	return crawlFetch{result: result, err: err, fetched: true}
}

func observedStatus(status string) bool {
	return status == protocol.StatusOK || status == protocol.StatusNotFound || status == protocol.StatusArchived
}

// SourceComplete means a full outgoing set was read, including confirmed absence.
func SourceComplete(node *Node) bool { return !node.Incomplete && observedStatus(node.Status) }
