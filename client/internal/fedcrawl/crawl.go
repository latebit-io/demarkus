package fedcrawl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
)

// FetchClient wraps the operations needed for crawling.
type FetchClient interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error)
}

// Crawler orchestrates multi-server federation crawling.
type Crawler struct {
	cfg    Config
	client FetchClient
	state  *State
	tokens *tokens.Store
	cred   tokens.Credential

	// Crawl results
	runMu          sync.Mutex
	mu             sync.Mutex
	hashes         map[string][]index.Entry // content-hash -> all observed locations
	servers        map[string]bool          // discovered servers (host)
	graph          *graph.Graph             // link graph accumulated during the walk (concurrency-safe itself)
	completedNodes []graphstore.StoredNode
	completedEdges []graphstore.StoredEdge
	publishable    bool
	publishSem     chan struct{}
}

// NewCrawler creates a new federation crawler.
// The state and tokenStore parameters are optional (may be nil).
// Crawler methods guard access via c.state != nil and c.tokens != nil checks.
func NewCrawler(cfg Config, client FetchClient, state *State, tokenStore *tokens.Store) *Crawler { //nolint:gocritic // hugeParam: Config by value is intentional for immutability
	return &Crawler{
		cfg:        cfg,
		client:     client,
		state:      state,
		tokens:     tokenStore,
		cred:       tokens.Credential{Origin: singleHubHost(cfg.Hubs)},
		hashes:     make(map[string][]index.Entry),
		servers:    make(map[string]bool),
		graph:      graph.New(),
		publishSem: make(chan struct{}, 1),
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
	c.runMu.Lock()
	defer c.runMu.Unlock()

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
	c.publishable = false
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
		host, err := links.DialHost(seed + "/")
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
	g := c.graph
	c.mu.Unlock()
	nodes, edges := snapshotGraph(g)
	c.mu.Lock()
	c.completedNodes = nodes
	c.completedEdges = edges
	c.publishable = !result.Incomplete
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
	walk := &serverWalk{c: c, run: run, host: host, authority: links.AuthorityURL(host), token: c.resolveToken(host)}
	depth := c.cfg.Crawl.MaxDepth
	if depth == 0 {
		depth = listwalk.RootOnly // the walker reads zero as its default
	}
	walker := listwalk.Walker{
		Client:   c.client,
		Host:     host,
		Token:    walk.token,
		MaxDepth: depth,
		// The LIST budget bounds breadth; MaxDocuments caps FETCHes, not listings.
		MaxLists:   max(c.cfg.Crawl.MaxDocuments, 100),
		BeforeList: c.pause,
		OnProblem:  walk.problem,
	}
	err := walker.Walk(ctx, "/", func(docPath string) error { return walk.visit(ctx, docPath) })
	return walk.count, err
}

// serverWalk is the state of one server's walk.
type serverWalk struct {
	c         *Crawler
	run       *crawlRun
	host      string
	authority string // host's identity, stamped on every entry it yields
	token     string
	count     int
}

// problem is the crawler's walk policy: a root that cannot be listed fails the
// server; anything below it is recorded and the walk carries on with siblings.
func (s *serverWalk) problem(p *listwalk.Problem) error {
	switch {
	case p.Kind == listwalk.ProblemInvalidEntry:
		s.run.recordIncomplete("server %s: invalid listing entry %q in %s", s.host, p.Entry, p.Dir)
	case p.Dir == "/":
		return p
	default:
		s.run.recordIncomplete("dir %s%s: %v", s.host, p.Dir, p)
	}
	return nil
}

// visit fetches one document, records its hash and edges, and queues the
// servers it links to. Only the document budget and cancellation end the walk.
func (s *serverWalk) visit(ctx context.Context, fullPath string) error {
	c := s.c
	// Bound attempts, not successes: failed peers must not bypass the cap.
	if int(s.run.fetchCount.Add(1)) > c.cfg.Crawl.MaxDocuments {
		s.run.fetchCount.Add(-1)
		return errors.New("document limit reached, crawl incomplete")
	}
	if err := c.pause(ctx); err != nil {
		return err
	}

	url := links.NodeURL(s.host, fullPath)
	doc, err := c.client.Fetch(ctx, fetch.FetchRequest{Host: s.host, Path: fullPath, Token: s.token})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if c.state != nil {
			c.state.RecordObservation(url, &protocol.Response{Status: "error"})
		}
		s.run.recordIncomplete("fetch %s%s: %v", s.host, fullPath, err)
		return nil
	}
	if c.state != nil {
		c.state.RecordObservation(url, &doc.Response)
	}
	if doc.Response.Status != protocol.StatusOK {
		s.run.recordIncomplete("fetch %s%s: status %s", s.host, fullPath, doc.Response.Status)
		return nil
	}

	contentHash := doc.Response.Metadata["content-hash"]
	if _, ok := protocol.IsHashPath(contentHash); ok {
		entry := index.Entry{Hash: contentHash, Server: s.authority, Path: fullPath}
		c.mu.Lock()
		c.hashes[contentHash] = append(c.hashes[contentHash], entry)
		c.mu.Unlock()
	} else {
		s.run.recordIncomplete("fetch %s%s: missing or invalid content-hash", s.host, fullPath)
	}

	// Generated graph exports are data, not authored discovery links.
	if !isGeneratedGraphPath(fullPath) {
		edges := c.recordEdges(s.host, fullPath, doc.Response.Body, doc.Response.Metadata)
		c.discoverServers(edges, s.host, s.run.queue, s.run.wg, s.run.recordIncomplete)
	}

	s.run.docCount.Add(1)
	s.count++
	return nil
}

func isGeneratedGraphPath(docPath string) bool {
	shardPrefix := graphstore.SnapshotShardRoot(graphstore.SnapshotManifestPath) + "/"
	return docPath == graphstore.LegacyExportPath || docPath == graphstore.SnapshotManifestPath || strings.HasPrefix(docPath, shardPrefix)
}

// pause waits out the politeness delay, or stops early when ctx ends: the
// request after it would be sent for nobody.
func (c *Crawler) pause(ctx context.Context) error {
	if c.cfg.Politeness.RequestDelay <= 0 {
		return nil
	}
	timer := time.NewTimer(c.cfg.Politeness.RequestDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// discoverServers queues foreign mark:// targets retained by graph policy.
func (c *Crawler) discoverServers(edges []graph.Edge, currentHost string, queue chan<- string, wg *sync.WaitGroup, recordIncomplete func(string, ...any)) {
	for _, edge := range edges {
		if !strings.HasPrefix(edge.To, "mark://") {
			continue
		}

		// Parse to extract host.
		host, err := links.DialHost(edge.To)
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
	authority := links.AuthorityURL(host)
	return slices.Contains(c.cfg.Hubs, authority) && !slices.Contains(c.cfg.Seeds, authority)
}

// singleHubHost returns the dial host DEMARKUS_AUTH belongs to: the hub when
// exactly one is configured, else none, so crawled hosts never receive it.
func singleHubHost(hubs []string) string {
	if len(hubs) != 1 {
		return ""
	}
	host, err := links.DialHost(hubs[0])
	if err != nil {
		return ""
	}
	return host
}

// resolveToken returns the auth token for a host.
func (c *Crawler) resolveToken(host string) string {
	return tokens.Resolve(c.cred, host, c.tokens)
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
	server := links.AuthorityURL(host)
	for _, entriesForHash := range c.hashes {
		for _, entry := range entriesForHash {
			if entry.Server == server {
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
		host, err := links.DialHost(hub + "/")
		if err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("parse hub URL %q: %w", hub, err))
			continue
		}

		token := c.resolveToken(host)

		if perServer {
			// Publish an index for each discovered server
			for serverHost := range c.servers {
				idxPath := "/index/" + serverHost + ".md"
				if err := c.publishShardedIndex(ctx, client, host, idxPath, links.AuthorityURL(serverHost), c.entriesForServer(serverHost), now, token); err != nil {
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

// Federation keeps mark:// topology only; source revisions survive hub export.
func (c *Crawler) recordEdges(host, docPath, body string, meta map[string]string) []graph.Edge {
	url := links.NodeURL(host, docPath)
	extracted := graph.ExtractDocumentEdges(url, body, meta)
	for _, rejected := range extracted.RejectedRels {
		slog.Warn("relation metadata skipped", "doc", url, "key", rejected.Key, "value", rejected.Value, "reason", rejected.Reason)
	}
	edges := make([]graph.Edge, 0, len(extracted.Edges))
	var linkCount int
	for _, edge := range extracted.Edges {
		target, ok := c.normalizeTarget(edge.To)
		if !ok {
			continue
		}
		edge.To = target
		c.graph.AddEdgeInfo(edge)
		edges = append(edges, edge)
		if edge.Rel == "" {
			linkCount++
		}
	}
	title := meta["title"]
	if title == "" {
		title = links.ExtractTitle(body)
	}
	observation := graph.Observe(url, meta)
	observation.View = graph.ViewFederation
	observation.Complete = true
	if c.state != nil {
		if previous := c.state.GetURL(url); previous != nil && previous.Observation.Revision > observation.Revision {
			observation.HighestRevision = previous.Observation.Revision
			observation.Problem = "revision-regression"
		}
	}
	c.graph.AddNode(&graph.Node{URL: url, Title: title, Status: "ok", LinkCount: linkCount, Observation: observation})
	return edges
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
	target, err := links.ParseMark(resolved)
	if err != nil || isLoopbackHost(target.DialHost()) {
		return "", false
	}
	return target.NodeURL(), true
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
	nodes, edges, _ := c.graphSnapshot()
	return graphstore.BuildExport(time.Now(), nodes, edges)
}

func (c *Crawler) graphSnapshot() ([]graphstore.StoredNode, []graphstore.StoredEdge, bool) {
	c.mu.Lock()
	nodes := append([]graphstore.StoredNode(nil), c.completedNodes...)
	edges := append([]graphstore.StoredEdge(nil), c.completedEdges...)
	publishable := c.publishable
	c.mu.Unlock()
	return nodes, edges, publishable
}

func snapshotGraph(g *graph.Graph) ([]graphstore.StoredNode, []graphstore.StoredEdge) {
	store := graphstore.New()
	store.Merge(g, nil)
	return store.Snapshot()
}

// PublishGraphToHubs publishes the link-graph export to each configured hub at
// /graph.md. Unlike the hash index this is always aggregated (cross-server
// edges are the whole point — they make portal nodes on the floor). Returns
// the number of successful publications.
func (c *Crawler) PublishGraphToHubs(ctx context.Context, client PublishClient) (int, error) {
	if len(c.cfg.Hubs) == 0 {
		return 0, nil
	}
	select {
	case c.publishSem <- struct{}{}:
		defer func() { <-c.publishSem }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	nodes, edges, publishable := c.graphSnapshot()
	if !publishable {
		return 0, errors.New("no complete graph generation available")
	}
	exported := time.Now().UTC()
	body := graphstore.BuildExport(exported, nodes, edges)
	successCount := 0
	var publishErrs []error

	for _, hub := range c.cfg.Hubs {
		host, err := links.DialHost(hub + "/")
		if err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("parse hub URL %q: %w", hub, err))
			continue
		}
		token := c.resolveToken(host)
		published := false
		if err := c.publishGraphSnapshot(ctx, client, host, nodes, edges, exported, token); err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("publish graph snapshot to hub %s: %w", hub, err))
		} else {
			published = true
		}
		if err := c.publishIndex(ctx, client, host, graphstore.LegacyExportPath, body, token); err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("publish /graph.md to hub %s: %w", hub, err))
		} else {
			published = true
		}
		if published {
			successCount++
		}
	}

	return successCount, errors.Join(publishErrs...)
}

func (c *Crawler) publishGraphSnapshot(ctx context.Context, client PublishClient, host string, nodes []graphstore.StoredNode, edges []graphstore.StoredEdge, exported time.Time, token string) error {
	_, err := graphstore.PublishSnapshot(ctx, graphstore.SnapshotPublishOptions{
		ManifestPath: graphstore.SnapshotManifestPath,
		Exported:     exported,
		Nodes:        nodes,
		Edges:        edges,
	}, func(ioCtx context.Context, docPath string) (protocol.Response, error) {
		result, err := client.Fetch(ioCtx, fetch.FetchRequest{Host: host, Path: docPath, Token: token})
		return result.Response, err
	}, func(ioCtx context.Context, docPath, body string, expectedVersion int) (protocol.Response, error) {
		meta := c.generatedArtifactMeta(docPath == graphstore.SnapshotManifestPath)
		result, err := client.Publish(ioCtx, fetch.WriteRequest{
			Host: host, Path: docPath, Token: token,
			Body: body, ExpectedVersion: expectedVersion, Metadata: meta,
		})
		return result.Response, err
	})
	return err
}

// PublishClient wraps the operations needed for publishing.
type PublishClient interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
}

func (c *Crawler) publishShardedIndex(ctx context.Context, client PublishClient, host, manifestPath, source string, entries []index.Entry, indexed time.Time, token string) error {
	_, err := index.PublishGeneration(ctx, index.PublishOptions{
		ManifestPath: manifestPath,
		Source:       source,
		Indexed:      indexed,
		Entries:      entries,
	}, func(ioCtx context.Context, docPath string) (protocol.Response, error) {
		result, err := client.Fetch(ioCtx, fetch.FetchRequest{Host: host, Path: docPath, Token: token})
		return result.Response, err
	}, func(ioCtx context.Context, docPath, body string, expectedVersion int) (protocol.Response, error) {
		meta := c.generatedArtifactMeta(docPath == manifestPath)
		result, err := client.Publish(ioCtx, fetch.WriteRequest{
			Host: host, Path: docPath, Token: token,
			Body: body, ExpectedVersion: expectedVersion, Metadata: meta,
		})
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
	result, err := client.Publish(ctx, fetch.WriteRequest{
		Host: host, Path: docPath, Token: token,
		Body: body, ExpectedVersion: -1, Metadata: meta,
	})
	if err != nil {
		return err
	}

	if status := result.Response.Status; !protocol.IsWriteSuccess(status) {
		return fmt.Errorf("publish returned %s", status)
	}

	return nil
}
