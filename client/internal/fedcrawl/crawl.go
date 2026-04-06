package fedcrawl

import (
	"context"
	"errors"
	"fmt"
	"log"

	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/index"
	"github.com/latebit/demarkus/client/internal/links"
	"github.com/latebit/demarkus/client/internal/tokens"
	"github.com/latebit/demarkus/protocol"
)

// FetchClient wraps the operations needed for crawling.
type FetchClient interface {
	Fetch(host, path, token string) (fetch.Result, error)
	List(host, path, token string) (fetch.Result, error)
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
	}
}

// CrawlResult holds the result of a crawl run.
type CrawlResult struct {
	ServersDiscovered int
	DocumentsCrawled  int
	HashesCollected   int
	Errors            []string
}

// Run executes the federation crawl starting from configured seeds.
// It discovers servers, collects content hashes, and returns results.
func (c *Crawler) Run(ctx context.Context) (*CrawlResult, error) {
	// Validate workers to prevent deadlock.
	if c.cfg.Crawl.Workers <= 0 {
		return nil, fmt.Errorf("crawl.workers must be > 0")
	}

	// Reset crawl state for each invocation.
	c.mu.Lock()
	c.hashes = make(map[string][]index.Entry)
	c.servers = make(map[string]bool)
	c.mu.Unlock()

	result := &CrawlResult{}

	// Buffer queue to handle discovery bursts. Size based on max servers.
	bufSize := max(c.cfg.Crawl.MaxServers, 100)
	queue := make(chan string, bufSize)
	var wg sync.WaitGroup

	var docCount atomic.Int32
	var errorsMu sync.Mutex
	var crawlErrors []string

	// Record error thread-safely.
	recordError := func(format string, args ...any) {
		errorsMu.Lock()
		crawlErrors = append(crawlErrors, fmt.Sprintf(format, args...))
		errorsMu.Unlock()
	}

	// Process servers from queue.
	worker := func() {
		for host := range queue {
			func() {
				defer wg.Done()

				// Check context cancellation.
				if ctx.Err() != nil {
					return
				}

				// Crawl this server.
				count, err := c.crawlServer(ctx, host, &docCount, queue, &wg, recordError)
				if err != nil {
					recordError("server %s: %v", host, err)
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
			recordError("invalid seed %q: %v", seed, err)
			continue
		}

		c.mu.Lock()
		if len(c.servers) >= c.cfg.Crawl.MaxServers {
			c.mu.Unlock()
			break // Stop seeding once we hit the cap
		}
		if c.servers[host] {
			c.mu.Unlock()
			continue
		}
		c.servers[host] = true // mark as queued
		c.mu.Unlock()

		wg.Add(1)
		queue <- host
	}

	// Wait for completion.
	wg.Wait()
	close(queue)

	// Save state.
	if c.state != nil {
		if err := c.state.Save(); err != nil {
			log.Printf("[WARN] save state: %v", err)
		}
	}

	// Build result.
	c.mu.Lock()
	result.ServersDiscovered = len(c.servers)
	result.DocumentsCrawled = int(docCount.Load())
	result.HashesCollected = len(c.hashes)
	result.Errors = crawlErrors
	c.mu.Unlock()

	return result, nil
}

// crawlServer crawls a single server, collecting hashes and discovering new servers.
// Returns the number of documents successfully crawled.
func (c *Crawler) crawlServer(ctx context.Context, host string, docCount *atomic.Int32, queue chan<- string, wg *sync.WaitGroup, recordError func(string, ...any)) (int, error) {
	token := c.resolveToken(host)
	var count int

	// Start from root.
	err := c.walkDir(ctx, host, "/", token, docCount, queue, wg, &count, recordError)
	return count, err
}

// walkDir recursively walks a directory on a server, collecting hashes and discovering links.
func (c *Crawler) walkDir(ctx context.Context, host, dirPath, token string, docCount *atomic.Int32, queue chan<- string, wg *sync.WaitGroup, count *int, recordError func(string, ...any)) error {
	// Check limits.
	if int(docCount.Load()) >= c.cfg.Crawl.MaxDocuments {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Apply politeness delay.
	if c.cfg.Politeness.RequestDelay > 0 {
		time.Sleep(c.cfg.Politeness.RequestDelay)
	}

	// List directory.
	result, err := c.client.List(host, dirPath, token)
	if err != nil {
		return fmt.Errorf("list %s: %w", dirPath, err)
	}
	if result.Response.Status != protocol.StatusOK {
		return nil // skip inaccessible directories
	}

	// Process each entry.
	for _, dest := range links.Extract(result.Response.Body) {
		// Check context cancellation.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Build full path.
		fullPath := dirPath
		if !strings.HasSuffix(fullPath, "/") {
			fullPath += "/"
		}
		fullPath += dest

		if strings.HasSuffix(dest, "/") {
			// Directory — recurse.
			if err := c.walkDir(ctx, host, fullPath, token, docCount, queue, wg, count, recordError); err != nil {
				return err
			}
			continue
		}

		// File — atomically reserve a slot before Fetch.
		newCount := int(docCount.Add(1))
		if newCount > c.cfg.Crawl.MaxDocuments {
			docCount.Add(-1) // Roll back reservation
			return nil
		}

		// Apply politeness delay.
		if c.cfg.Politeness.RequestDelay > 0 {
			time.Sleep(c.cfg.Politeness.RequestDelay)
		}

		doc, err := c.client.Fetch(host, fullPath, token)
		if err != nil {
			docCount.Add(-1) // Roll back reservation
			recordError("fetch %s%s: %v", host, fullPath, err)
			continue
		}

		url := "mark://" + host + fullPath

		// Record visit.
		if c.state != nil {
			etag := doc.Response.Metadata["etag"]
			contentHash := doc.Response.Metadata["content-hash"]
			c.state.RecordVisit(url, etag, doc.Response.Status, contentHash)
		}

		if doc.Response.Status != protocol.StatusOK {
			docCount.Add(-1) // Roll back reservation
			continue
		}

		// Collect content hash.
		contentHash := doc.Response.Metadata["content-hash"]
		if _, ok := protocol.IsHashPath(contentHash); ok {
			entry := index.Entry{
				Hash:   contentHash,
				Server: "mark://" + host,
				Path:   fullPath,
			}
			c.mu.Lock()
			c.hashes[contentHash] = append(c.hashes[contentHash], entry)
			c.mu.Unlock()
		}

		// Discover cross-server links.
		c.discoverServers(doc.Response.Body, host, queue, wg)

		*count++
	}

	return nil
}

// discoverServers extracts mark:// links pointing to other servers and queues them.
func (c *Crawler) discoverServers(body, currentHost string, queue chan<- string, wg *sync.WaitGroup) {
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

		// Skip if same server.
		if host == currentHost {
			continue
		}

		// Check if we should queue this host.
		// Only hold mutex while checking/updating shared state.
		c.mu.Lock()
		if len(c.servers) >= c.cfg.Crawl.MaxServers {
			c.mu.Unlock()
			return // Stop discovering once we hit the limit
		}
		newHost := !c.servers[host]
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

// resolveToken returns the auth token for a host.
func (c *Crawler) resolveToken(host string) string {
	if c.tokens == nil {
		return ""
	}
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

// IndexForServer builds a hash index document for a specific server.
func (c *Crawler) IndexForServer(host string, indexed time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var entries []index.Entry
	for _, entriesForHash := range c.hashes {
		for _, e := range entriesForHash {
			if e.Server == "mark://"+host {
				entries = append(entries, e)
			}
		}
	}

	// Sort by path for deterministic output.
	slices.SortFunc(entries, func(a, b index.Entry) int {
		return strings.Compare(a.Path, b.Path)
	})

	return index.Build("mark://"+host, indexed, entries)
}

// GlobalIndex builds a combined hash index document with entries from all servers.
func (c *Crawler) GlobalIndex(indexed time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var entries []index.Entry
	for _, entriesForHash := range c.hashes {
		entries = append(entries, entriesForHash...)
	}
	slices.SortFunc(entries, func(a, b index.Entry) int {
		if cmp := strings.Compare(a.Hash, b.Hash); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Server, b.Server)
	})

	// Use empty source since this is aggregated.
	return index.Build("aggregated", indexed, entries)
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
				idxBody := c.IndexForServer(serverHost, now)
				idxPath := "/index/" + serverHost + ".md"

				if err := c.publishIndex(ctx, client, host, idxPath, idxBody, token); err != nil {
					publishErrs = append(publishErrs, fmt.Errorf("publish %s to hub %s: %w", idxPath, hub, err))
					continue
				}
				successCount++
			}
		} else {
			// Publish a single aggregated index
			idxBody := c.GlobalIndex(now)
			idxPath := "/index.md"

			if err := c.publishIndex(ctx, client, host, idxPath, idxBody, token); err != nil {
				publishErrs = append(publishErrs, fmt.Errorf("publish %s to hub %s: %w", idxPath, hub, err))
				continue
			}
			successCount++
		}
	}

	return successCount, errors.Join(publishErrs...)
}

// PublishClient wraps the operations needed for publishing.
type PublishClient interface {
	Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
}

// publishIndex publishes a single index document to a hub.
func (c *Crawler) publishIndex(ctx context.Context, client PublishClient, host, path, body, token string) error {
	// Check context
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// For now, we create new indexes (expected_version=0)
	// In the future, we could fetch existing and merge
	result, err := client.Publish(host, path, body, token, 0, map[string]string{
		"agent": "demarkus-agent",
	})
	if err != nil {
		return err
	}

	if result.Response.Status != protocol.StatusOK {
		return fmt.Errorf("publish returned %s", result.Response.Status)
	}

	return nil
}
