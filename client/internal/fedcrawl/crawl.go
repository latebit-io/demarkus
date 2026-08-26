package fedcrawl

import (
	"context"
	"errors"
	"fmt"
	"net"

	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/listing"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
)

// FetchClient wraps the operations needed for crawling.
type FetchClient interface {
	Fetch(host, path, token string) (fetch.Result, error)
	ListWithOptions(host, path, token string, opts fetch.ListOptions) (fetch.Result, error)
}

// Crawler orchestrates multi-server federation crawling.
type Crawler struct {
	cfg    Config
	client FetchClient
	state  *State
	tokens *tokens.Store

	// Crawl results
	mu      sync.Mutex
	hashes  map[string][]index.Entry // content-hash -> all observed locations
	servers map[string]bool          // discovered servers (host)
	graph   *graph.Graph             // link graph accumulated during the walk (concurrency-safe itself)
}

// NewCrawler creates a new federation crawler.
// The state and tokenStore parameters are optional (may be nil).
// Crawler methods guard access via c.state != nil and c.tokens != nil checks.
func NewCrawler(cfg Config, client FetchClient, state *State, tokenStore *tokens.Store) *Crawler { //nolint:gocritic // hugeParam: Config by value is intentional for immutability
	return &Crawler{
		cfg:     cfg,
		client:  client,
		state:   state,
		tokens:  tokenStore,
		hashes:  make(map[string][]index.Entry),
		servers: make(map[string]bool),
		graph:   graph.New(),
	}
}

// CrawlResult holds the result of a crawl run.
type CrawlResult struct {
	ServersDiscovered int
	DocumentsCrawled  int
	HashesCollected   int
	Incomplete        bool
	Errors            []string
}

// Run executes the federation crawl starting from configured seeds.
// It discovers servers, collects content hashes, and returns results.
func (c *Crawler) Run(ctx context.Context) (*CrawlResult, error) {
	// Run is the package boundary, so enforce normalization even when callers
	// construct a Crawler directly instead of using the agent command.
	if err := c.cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid crawler config: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Reset crawl state for each invocation.
	c.mu.Lock()
	c.hashes = make(map[string][]index.Entry)
	c.servers = make(map[string]bool)
	c.graph = graph.New()
	c.mu.Unlock()

	result := &CrawlResult{}

	// Buffer queue to handle discovery bursts. Size based on max servers.
	bufSize := max(c.cfg.Crawl.MaxServers, 100)
	queue := make(chan string, bufSize)
	var wg sync.WaitGroup

	var docCount atomic.Int32
	var fetchCount atomic.Int32
	var incomplete atomic.Bool
	var errorsMu sync.Mutex
	var crawlErrors []string

	// Record error thread-safely.
	recordError := func(format string, args ...any) {
		errorsMu.Lock()
		crawlErrors = append(crawlErrors, fmt.Sprintf(format, args...))
		errorsMu.Unlock()
	}
	recordIncomplete := func(format string, args ...any) {
		incomplete.Store(true)
		recordError(format, args...)
	}

	run := &crawlRun{docCount: &docCount, fetchCount: &fetchCount, queue: queue, wg: &wg, recordIncomplete: recordIncomplete}

	// Process servers from queue.
	worker := func() {
		for host := range queue {
			func() {
				defer wg.Done()

				// Check context cancellation.
				if err := ctx.Err(); err != nil {
					recordIncomplete("server %s: %v", host, err)
					return
				}

				// Crawl this server.
				count, err := c.crawlServer(ctx, run, host)
				if err != nil {
					recordIncomplete("server %s: %v", host, err)
					return
				}

				c.mu.Lock()
				c.servers[host] = true
				c.mu.Unlock()

				if c.state != nil {
					c.state.RecordServer(host, count)
				}
			}()
		}
	}

	// Start workers.
	for range c.cfg.Crawl.Workers {
		go worker()
	}

	// Seed the queue.
	for _, seed := range c.cfg.Seeds {
		host, _, err := fetch.ParseMarkURL(seed + "/")
		if err != nil {
			recordIncomplete("invalid seed %q: %v", seed, err)
			continue
		}

		c.mu.Lock()
		if c.servers[host] {
			c.mu.Unlock()
			continue
		}
		if len(c.servers) >= c.cfg.Crawl.MaxServers {
			c.mu.Unlock()
			recordIncomplete("server limit reached while seeding %q, crawl incomplete", seed)
			break // Stop seeding once we hit the cap
		}
		c.servers[host] = true // mark as queued
		c.mu.Unlock()

		wg.Add(1)
		queue <- host
	}

	// Wait for completion.
	wg.Wait()
	close(queue)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Save state.
	if c.state != nil {
		if err := c.state.Save(); err != nil {
			recordError("save state: %v", err)
		}
	}

	// Build result.
	c.mu.Lock()
	result.ServersDiscovered = len(c.servers)
	result.DocumentsCrawled = int(docCount.Load())
	result.HashesCollected = len(c.hashes)
	result.Incomplete = incomplete.Load()
	result.Errors = crawlErrors
	c.mu.Unlock()

	return result, nil
}

// crawlRun bundles the run-wide state shared by every server walk in one
// Run invocation.
type crawlRun struct {
	docCount         *atomic.Int32
	fetchCount       *atomic.Int32
	queue            chan<- string
	wg               *sync.WaitGroup
	recordIncomplete func(string, ...any)
}

// crawlServer crawls a single server, collecting hashes and discovering new servers.
// Returns the number of documents successfully crawled.
func (c *Crawler) crawlServer(ctx context.Context, run *crawlRun, host string) (int, error) {
	walk := &serverWalk{
		c:       c,
		run:     run,
		host:    host,
		token:   c.resolveToken(host),
		visited: make(map[string]bool),
	}

	// Start from root.
	err := walk.walkDir(ctx, "/", 0)
	return walk.count, err
}

// serverWalk carries the per-server walk state so it isn't threaded as
// parameters through every recursion level.
type serverWalk struct {
	c       *Crawler
	run     *crawlRun
	host    string
	token   string
	count   int
	lists   int             // LIST requests issued to this server
	visited map[string]bool // normalized paths, dedups repeated references
}

// walkDir recursively walks a directory on a server, collecting hashes and discovering links.
func (s *serverWalk) walkDir(ctx context.Context, dirPath string, depth int) error {
	c := s.c

	// MaxDepth is the cycle-safety bound: a self-referencing listing mints
	// ever-deeper distinct paths the visited set cannot catch. The LIST
	// budget bounds breadth (MaxDocuments caps FETCHes, not listings).
	if depth > c.cfg.Crawl.MaxDepth {
		return errors.New("depth limit reached, crawl incomplete")
	}
	if s.visited[dirPath] {
		return nil
	}
	s.visited[dirPath] = true
	return s.walkDirectoryPages(ctx, dirPath, depth)
}

func (s *serverWalk) walkEntries(ctx context.Context, entries []listing.Entry, depth int) error {
	c := s.c
	for _, entry := range entries {
		// Check context cancellation.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fullPath := entry.Path

		if entry.IsDir {
			// Directory — recurse.
			if err := s.walkDir(ctx, fullPath+"/", depth+1); err != nil {
				// Only abort on cancellation; record other errors and continue.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				s.run.recordIncomplete("dir %s%s: %v", s.host, fullPath, err)
			}
			continue
		}

		if s.visited[fullPath] {
			continue
		}
		s.visited[fullPath] = true

		// Bound attempts, not successes: failed peers must not bypass the cap.
		newCount := int(s.run.fetchCount.Add(1))
		if newCount > c.cfg.Crawl.MaxDocuments {
			s.run.fetchCount.Add(-1)
			return errors.New("document limit reached, crawl incomplete")
		}

		// Apply politeness delay.
		if c.cfg.Politeness.RequestDelay > 0 {
			time.Sleep(c.cfg.Politeness.RequestDelay)
		}

		doc, err := c.client.Fetch(s.host, fullPath, s.token)
		if err != nil {
			s.run.recordIncomplete("fetch %s%s: %v", s.host, fullPath, err)
			continue
		}

		url := "mark://" + s.host + fullPath

		// Record visit.
		if c.state != nil {
			etag := doc.Response.Metadata["etag"]
			contentHash := doc.Response.Metadata["content-hash"]
			c.state.RecordVisit(url, etag, doc.Response.Status, contentHash)
		}

		if doc.Response.Status != protocol.StatusOK {
			s.run.recordIncomplete("fetch %s%s: status %s", s.host, fullPath, doc.Response.Status)
			continue
		}

		// Collect content hash.
		contentHash := doc.Response.Metadata["content-hash"]
		if _, ok := protocol.IsHashPath(contentHash); ok {
			entry := index.Entry{
				Hash:   contentHash,
				Server: "mark://" + s.host,
				Path:   fullPath,
			}
			c.mu.Lock()
			c.hashes[contentHash] = append(c.hashes[contentHash], entry)
			c.mu.Unlock()
		} else {
			s.run.recordIncomplete("fetch %s%s: missing or invalid content-hash", s.host, fullPath)
		}

		// /graph.md is a generated projection of other documents' edges. Hash it
		// like any document, but do not turn its table links into synthetic
		// graph.md→node edges or use them to amplify the discovery frontier.
		if fullPath != "/graph.md" {
			c.recordEdges(s.host, fullPath, doc.Response.Body, doc.Response.Metadata)
			c.discoverServers(doc.Response.Body, s.host, s.run.queue, s.run.wg, s.run.recordIncomplete)
		}

		s.run.docCount.Add(1)
		s.count++
	}

	return nil
}

func (s *serverWalk) walkDirectoryPages(ctx context.Context, dirPath string, depth int) error {
	limit := max(s.c.cfg.Crawl.MaxDocuments, 100)
	cursor := ""
	seenCursors := make(map[string]struct{})
	lastName := ""
	for {
		if s.lists >= limit {
			return fmt.Errorf("list budget exhausted at %s, crawl incomplete", dirPath)
		}
		s.lists++
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.c.cfg.Politeness.RequestDelay > 0 {
			time.Sleep(s.c.cfg.Politeness.RequestDelay)
		}

		result, err := s.c.client.ListWithOptions(s.host, dirPath, s.token, fetch.ListOptions{
			Cursor:   cursor,
			PageSize: protocol.MaxListPageSize,
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", dirPath, err)
		}
		if result.Response.Status != protocol.StatusOK {
			return fmt.Errorf("list %s: status %s, crawl incomplete", dirPath, result.Response.Status)
		}
		page, err := listing.ParsePage(dirPath, result.Response, lastName)
		if err != nil {
			return fmt.Errorf("list %s: %w", dirPath, err)
		}
		for _, dest := range page.Invalid {
			s.run.recordIncomplete("server %s: invalid listing entry %q in %s", s.host, dest, dirPath)
		}
		lastName = page.LastName
		if err := s.walkEntries(ctx, page.Entries, depth); err != nil {
			return err
		}
		if page.Complete {
			return nil
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate || page.NextCursor == cursor {
			return fmt.Errorf("list %s: continuation cursor did not advance", dirPath)
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
}

// discoverServers extracts mark:// links pointing to other servers and queues them.
func (c *Crawler) discoverServers(body, currentHost string, queue chan<- string, wg *sync.WaitGroup, recordIncomplete func(string, ...any)) {
	for _, link := range links.Extract(body) {
		// Resolve relative links.
		resolved := links.Resolve("mark://"+currentHost, link)

		// Only follow mark:// links.
		if !strings.HasPrefix(resolved, "mark://") {
			continue
		}

		// Parse to extract host.
		host, _, err := fetch.ParseMarkURL(resolved)
		if err != nil {
			continue
		}

		// Don't crawl loopback/localhost — a dev link in a crawled body points
		// at the crawler's own host, never a real federated world (unreachable
		// noise + error spam).
		if isLoopbackHost(host) {
			continue
		}

		// Skip if same server.
		if host == currentHost {
			continue
		}
		// A hub is an aggregation destination, not crawl input, unless the
		// operator also listed it as a seed. Its edge remains in the graph.
		if c.isPublishOnlyHub(host) {
			continue
		}

		// Check if we should queue this host.
		// Only hold mutex while checking/updating shared state.
		c.mu.Lock()
		newHost := !c.servers[host]
		if newHost && len(c.servers) >= c.cfg.Crawl.MaxServers {
			c.mu.Unlock()
			recordIncomplete("server limit reached at %s, crawl incomplete", host)
			return // Stop discovering once we hit the limit
		}
		if newHost {
			c.servers[host] = true
		}
		c.mu.Unlock()

		// Enqueue outside the mutex to avoid blocking.
		if newHost {
			wg.Add(1)
			queue <- host
		}
	}
}

func (c *Crawler) isPublishOnlyHub(host string) bool {
	authority := "mark://" + host
	return slices.Contains(c.cfg.Hubs, authority) && !slices.Contains(c.cfg.Seeds, authority)
}

// resolveToken returns the auth token for a host.
func (c *Crawler) resolveToken(host string) string {

	return tokens.Resolve("", host, c.tokens)
}

// Hashes returns all collected content hashes flattened to single entries.
// When the same content appears at multiple locations, the first location is returned.
func (c *Crawler) Hashes() map[string]index.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make(map[string]index.Entry, len(c.hashes))
	for hash, entries := range c.hashes {
		if len(entries) > 0 {
			cp[hash] = entries[0]
		}
	}
	return cp
}

func (c *Crawler) entriesForServer(host string) []index.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var entries []index.Entry
	for _, entriesForHash := range c.hashes {
		for _, entry := range entriesForHash {
			if entry.Server == "mark://"+host {
				entries = append(entries, entry)
			}
		}
	}
	slices.SortFunc(entries, func(a, b index.Entry) int {
		return strings.Compare(a.Path, b.Path)
	})
	return entries
}

func (c *Crawler) globalEntries() []index.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, entriesForHash := range c.hashes {
		total += len(entriesForHash)
	}
	entries := make([]index.Entry, 0, total)
	for _, entriesForHash := range c.hashes {
		entries = append(entries, entriesForHash...)
	}
	slices.SortFunc(entries, func(a, b index.Entry) int {
		if cmp := strings.Compare(a.Hash, b.Hash); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.Server, b.Server); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Path, b.Path)
	})

	return entries
}

// PublishToHubs publishes indexes to all configured hubs.
// If perServer is true, publishes a separate index for each discovered server.
// If perServer is false, publishes a single aggregated index.
// Returns the number of successful publications.
func (c *Crawler) PublishToHubs(ctx context.Context, client PublishClient, perServer bool) (int, error) {
	if len(c.cfg.Hubs) == 0 {
		return 0, nil
	}

	now := time.Now().UTC()
	successCount := 0
	var publishErrs []error

	for _, hub := range c.cfg.Hubs {
		host, _, err := fetch.ParseMarkURL(hub + "/")
		if err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("parse hub URL %q: %w", hub, err))
			continue
		}

		token := c.resolveToken(host)

		if perServer {
			// Publish an index for each discovered server
			for serverHost := range c.servers {
				idxPath := "/index/" + serverHost + ".md"
				if err := c.publishShardedIndex(ctx, client, host, idxPath, "mark://"+serverHost, c.entriesForServer(serverHost), now, token); err != nil {
					publishErrs = append(publishErrs, fmt.Errorf("publish %s to hub %s: %w", idxPath, hub, err))
					continue
				}
				successCount++
			}
		} else {
			// Publish a single aggregated index
			idxPath := "/index.md"
			if err := c.publishShardedIndex(ctx, client, host, idxPath, "aggregated", c.globalEntries(), now, token); err != nil {
				publishErrs = append(publishErrs, fmt.Errorf("publish %s to hub %s: %w", idxPath, hub, err))
				continue
			}
			successCount++
		}
	}

	return successCount, errors.Join(publishErrs...)
}

// recordEdges adds a crawled document and its outbound mark:// links to the
// link graph. Only mark:// targets are kept (the floor's edges are between
// demarkus documents); external links are left out. The graph is the source
// for the hub graph export — the durable, transport-symmetric topology the
// reading-room floor renders (plans "Floor enrichment", decision 11).
func (c *Crawler) recordEdges(host, docPath, body string, meta map[string]string) {
	url := "mark://" + host + docPath
	base := "mark://" + host
	var linkCount int
	for _, l := range mdoutline.AnchoredLinks(body) {
		target, ok := c.normalizeTarget(links.Resolve(base, l.Dest))
		if !ok {
			continue
		}
		c.graph.AddEdgeInfo(graph.Edge{From: url, To: target, Label: l.Label, Anchor: l.Anchor, Count: 1})
		linkCount++
	}
	// Typed relations from rel-<predicate> metadata, filtered like body links.
	for _, r := range graph.RelEdges(url, meta) {
		target, ok := c.normalizeTarget(r.Target)
		if !ok {
			continue
		}
		c.graph.AddEdgeInfo(graph.Edge{From: url, To: target, Rel: r.Rel, Count: 1})
	}
	// Declared metadata title first, H1 fallback like mark_graph's crawler:
	// most docs carry their title as a heading, not metadata, and a blank
	// title here propagates to every hub-graph consumer.
	title := meta["title"]
	if title == "" {
		title = links.ExtractTitle(body)
	}
	c.graph.AddNode(&graph.Node{URL: url, Title: title, Status: "ok", LinkCount: linkCount})
}

// normalizeTarget keeps only mark:// targets with a port-stable host
// (mark://h and mark://h:6309 are the same node, not two) and drops
// loopback/localhost so a dev link in a crawled body never becomes a phantom
// portal node on the reading-room floor. Private and cluster-internal hosts
// are KEPT; real federated worlds (LAN, or a Kubernetes universe) are
// addressed by exactly those.
func (c *Crawler) normalizeTarget(resolved string) (string, bool) {
	if !strings.HasPrefix(resolved, "mark://") {
		return "", false
	}
	th, tp, err := fetch.ParseMarkURL(resolved)
	if err != nil || isLoopbackHost(th) {
		return "", false
	}
	return "mark://" + th + tp, true
}

// isLoopbackHost reports whether a mark:// host (host:port or bare) is loopback,
// "localhost", or the unspecified address — dev artifacts that must not enter
// the durable federation graph or the crawl frontier. Private and
// cluster-internal hosts are deliberately NOT included: real federated worlds (a
// LAN deployment, or a Kubernetes universe addressing worlds by in-cluster
// service names) are reached by exactly those.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip the port. SplitHostPort handles host:port and [ipv6]:port; an
	// unbracketed IPv6 with a port (::1:6309) defeats it (the colons are
	// ambiguous), so fall back to trimming a trailing :port only when the head
	// is itself a valid IP.
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	} else if i := strings.LastIndex(h, ":"); i > 0 && net.ParseIP(h[:i]) != nil {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// GraphExport renders the accumulated link graph as a mark_graph_export
// document (Nodes + Edges tables). It reuses graphstore's exporter so the
// format stays identical to mark_graph_export / mark_graph_publish — the same
// document any agent or the library floor reads.
func (c *Crawler) GraphExport() string {
	// Snapshot the graph pointer under the lock: Run reassigns c.graph under
	// c.mu, so an export racing a re-crawl must not read the field unguarded
	// (the graph's own methods are synchronized; the field reassignment is not).
	c.mu.Lock()
	g := c.graph
	c.mu.Unlock()
	store := graphstore.New()
	store.Merge(g, nil)
	return store.Export()
}

// PublishGraphToHubs publishes the link-graph export to each configured hub at
// /graph.md. Unlike the hash index this is always aggregated (cross-server
// edges are the whole point — they make portal nodes on the floor). Returns
// the number of successful publications.
func (c *Crawler) PublishGraphToHubs(ctx context.Context, client PublishClient) (int, error) {
	if len(c.cfg.Hubs) == 0 {
		return 0, nil
	}

	body := c.GraphExport()
	successCount := 0
	var publishErrs []error

	for _, hub := range c.cfg.Hubs {
		host, _, err := fetch.ParseMarkURL(hub + "/")
		if err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("parse hub URL %q: %w", hub, err))
			continue
		}
		token := c.resolveToken(host)
		if err := c.publishIndex(ctx, client, host, "/graph.md", body, token); err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("publish /graph.md to hub %s: %w", hub, err))
			continue
		}
		successCount++
	}

	return successCount, errors.Join(publishErrs...)
}

// PublishClient wraps the operations needed for publishing.
type PublishClient interface {
	Fetch(host, path, token string) (fetch.Result, error)
	FetchContext(ctx context.Context, host, path, token string) (fetch.Result, error)
	Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	PublishContext(ctx context.Context, host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
}

func (c *Crawler) publishShardedIndex(ctx context.Context, client PublishClient, host, manifestPath, source string, entries []index.Entry, indexed time.Time, token string) error {
	_, err := index.PublishGeneration(ctx, index.PublishOptions{
		ManifestPath: manifestPath,
		Source:       source,
		Indexed:      indexed,
		Entries:      entries,
	}, func(ioCtx context.Context, docPath string) (protocol.Response, error) {
		result, err := client.FetchContext(ioCtx, host, docPath, token)
		return result.Response, err
	}, func(ioCtx context.Context, docPath, body string, expectedVersion int) (protocol.Response, error) {
		meta := c.generatedArtifactMeta(docPath == manifestPath)
		result, err := client.PublishContext(ioCtx, host, docPath, body, token, expectedVersion, meta)
		return result.Response, err
	})
	return err
}

func (c *Crawler) generatedArtifactMeta(retain bool) map[string]string {
	meta := map[string]string{
		"agent": "demarkus-agent",
		"tags":  "category:federation",
		"type":  "Reference",
	}
	if retain && c.cfg.Publish.Retention > 0 {
		meta["retention"] = strconv.Itoa(c.cfg.Publish.Retention)
	}
	return meta
}

// publishIndex publishes a generated whole document, currently /graph.md.
// Duplicate bodies create no version, keeping unconditional writes idempotent.
func (c *Crawler) publishIndex(ctx context.Context, client PublishClient, host, docPath, body, token string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	meta := c.generatedArtifactMeta(true)
	// Retention bounds generated whole-document history (SPEC §9.9).
	result, err := client.Publish(host, docPath, body, token, -1, meta)
	if err != nil {
		return err
	}

	status := result.Response.Status
	if status != protocol.StatusOK && status != protocol.StatusCreated {
		return fmt.Errorf("publish returned %s", status)
	}

	return nil
}
