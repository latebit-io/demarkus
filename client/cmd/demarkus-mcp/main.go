// Command demarkus-mcp is an MCP server that exposes the Mark Protocol as tools
// for LLM agents. It supports fetching documents, listing directories, and
// crawling link graphs via stdio transport.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/client/internal/cache"
	"github.com/latebit-io/demarkus/client/internal/listwalk"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	defaultHost := flag.String("host", "", "default Mark server (e.g. mark://localhost:6309)")
	token := flag.String("token", "", "auth token for capability-based authentication")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	noCache := flag.Bool("no-cache", false, "disable response caching")
	cacheDir := flag.String("cache-dir", cache.DefaultDir(), "cache directory")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	opts := fetch.Options{Insecure: *insecure}
	if !*noCache {
		opts.Cache = cache.New(*cacheDir)
	}
	client := fetch.NewClient(opts)
	defer client.Close()

	// listChanged on resources: the startup listing registers picker
	// entries in the background after clients may have connected, and the
	// notification is what makes them appear without a reconnect.
	s := mcpserver.NewMCPServer("demarkus-mcp", version,
		mcpserver.WithResourceCapabilities(false, true),
	)

	gs, gsErr := graphstore.Load(graphstore.DefaultPath())
	if gsErr != nil {
		log.Printf("warning: graph store unavailable: %v", gsErr)
	}
	h := &handler{client: client, defaultHost: *defaultHost, token: *token, graphStore: gs}
	s.AddTool(markFetchTool(*defaultHost), h.markFetch)
	s.AddTool(markExploreTool(*defaultHost), h.markExplore)
	s.AddTool(markListTool(*defaultHost), h.markList)
	s.AddTool(markGraphTool(*defaultHost), h.markGraph)
	s.AddTool(markVersionsTool(*defaultHost), h.markVersions)
	s.AddTool(markLookupTool(*defaultHost), h.markLookup)
	s.AddTool(markPublishTool(*defaultHost), h.markPublish)
	s.AddTool(markArchiveTool(*defaultHost), h.markArchive)
	s.AddTool(markAppendTool(*defaultHost), h.markAppend)
	s.AddTool(markDiscoverTool(*defaultHost), h.markDiscover)
	s.AddTool(markResolveTool(*defaultHost), h.markResolve)
	s.AddTool(markIndexTool(*defaultHost), h.markIndex)
	s.AddTool(markBacklinksTool(*defaultHost), h.markBacklinks)
	s.AddTool(markGraphExportTool(), h.markGraphExport)
	s.AddTool(markGraphPublishTool(*defaultHost), h.markGraphPublish)

	registerResources(s, h, *defaultHost)
	registerPrompts(s, *defaultHost)
	if *defaultHost != "" {
		// Best-effort picker population; never blocks or fails startup.
		go registerListedResources(s, h, *defaultHost)
	}

	if err := mcpserver.ServeStdio(s); err != nil {
		log.Fatal(err)
	}
}

// markClient defines the fetch operations used by MCP tool handlers.
type markClient interface {
	Fetch(host, path, token string) (fetch.Result, error)
	FetchConditional(host, path, token, etag string) (fetch.Result, error)
	List(host, path, token string) (fetch.Result, error)
	ListWithOptions(host, path, token string, opts fetch.ListOptions) (fetch.Result, error)
	Versions(host, path, token string) (fetch.Result, error)
	Lookup(host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error)
	Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	Append(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	Archive(host, path, token string) (fetch.Result, error)
}

type handler struct {
	client      markClient
	defaultHost string
	token       string
	graphStore  *graphstore.Store

	// seenMu guards seen, the per-session fetch dedup state: host+path →
	// identity of the document version whose full body was already returned
	// to the agent this session. Process-lifetime only, never persisted —
	// cross-session staleness risk outweighs the token saving. Identity
	// semantics and notice texts live in client/fetchdedup, shared with the
	// broker MCP gateway so the two surfaces answer identically.
	seenMu sync.Mutex
	seen   map[string]fetchdedup.Doc

	// seedMu guards seedChecked: host → last /graph.md seed check, the
	// per-process throttle for seedGraph. Process-scoped like seen.
	seedMu      sync.Mutex
	seedChecked map[string]time.Time
}

// seenLookup returns the recorded identity for key, if any.
func (h *handler) seenLookup(key string) (fetchdedup.Doc, bool) {
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	d, ok := h.seen[key]
	return d, ok
}

// seenRecord remembers that the full body of key's document was returned.
func (h *handler) seenRecord(key string, d fetchdedup.Doc) {
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	if h.seen == nil {
		h.seen = make(map[string]fetchdedup.Doc)
	}
	h.seen[key] = d
}

// seedCheckInterval caps /graph.md seed checks at one per host per interval;
// the first graph-tool call in a session always checks.
const seedCheckInterval = 5 * time.Minute

// seedGraph refreshes the local graph store from the published /graph.md of
// the world at host (canonical host:port, as returned by resolveURL) so
// backlink queries answer before any local crawl. Local crawls win (see
// graphstore.SeedFromExport). Never fatal: every failure degrades silently
// to the unseeded store, warn-logging real errors.
func (h *handler) seedGraph(host string) {
	if h.graphStore == nil || h.client == nil || host == "" {
		return
	}
	const path = "/graph.md"

	h.seedMu.Lock()
	if last, ok := h.seedChecked[host]; ok && time.Since(last) < seedCheckInterval {
		h.seedMu.Unlock()
		return
	}
	if h.seedChecked == nil {
		h.seedChecked = make(map[string]time.Time)
	}
	h.seedChecked[host] = time.Now()
	h.seedMu.Unlock()

	result, err := h.client.FetchConditional(host, path, h.resolveToken(host), h.graphStore.SeedEtag(host))
	if err != nil {
		log.Printf("warning: graph seed fetch mark://%s%s: %v", host, path, err)
		return
	}
	// not-modified means the stored seed is current; any other non-ok status
	// (a world without /graph.md is normal) means nothing to seed.
	if result.Response.Status != protocol.StatusOK {
		return
	}

	nodes, edges := graphstore.ParseExport(result.Response.Body)
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	h.graphStore.SeedFromExport(nodes, edges)
	if etag := result.Response.Metadata["etag"]; etag != "" {
		h.graphStore.SetSeedEtag(host, etag)
	}
	if err := h.graphStore.Save(); err != nil {
		log.Printf("warning: graph seed save: %v", err)
	}
}

// resolveToken returns the auth token for a host using the shared cascade:
// explicit -token flag > DEMARKUS_AUTH env var > stored token for host.
// Reloads the token store on each call so changes on disk are picked up
// without restarting the MCP server.
func (h *handler) resolveToken(host string) string {
	return tokens.Resolve(h.token, host, tokens.LoadDefault())
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
				"Returns the document status, version, modified timestamp, etag, and markdown body. "+
				"Documents under 8KB return the full body. Larger documents return an outline "+
				"instead: the heading tree with #anchors and per-section line counts plus the "+
				"opening paragraph, so you can pull just the section you need by appending "+
				"#<anchor> to the url (anchors are GitHub-style slugs; #section fetches work at "+
				"any size). Re-fetching a document whose full body was already returned this "+
				"session returns a short 'unchanged' notice when it has not changed. "+
				"Set force=true for the full body regardless of size or session history. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)+"; append #<anchor> to fetch a single section"),
		),
		mcp.WithBoolean("force",
			mcp.Description("return the full body even if the document is large or unchanged since an earlier fetch this session (default false)"),
		),
	)
}

// outlineThreshold is the body size (bytes) above which mark_fetch returns
// an outline instead of the full body, unless force=true or a #section is
// requested.
const outlineThreshold = 8 * 1024

func markListTool(host string) mcp.Tool {
	return mcp.NewTool("mark_list",
		mcp.WithDescription(
			"List documents and subdirectories on a Mark Protocol server. "+
				"Use this to discover what documents exist. To orient around one "+
				"specific document, prefer mark_explore: it bundles the sibling "+
				"listing with the outline, links, and backlinks. Archived documents are "+
				"hidden by default, along with directories that contain only archived "+
				"documents; set include_archived to true for a recovery/audit view. "+
				"Results declare complete; when false, pass next-cursor back as cursor. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
		mcp.WithBoolean("include_archived",
			mcp.Description("Include archived documents (and all-archived directories) in the listing. Default false."),
		),
		mcp.WithString("cursor",
			mcp.Description("opaque continuation cursor from a prior mark_list result"),
		),
		mcp.WithNumber("page_size",
			mcp.Description("maximum entries in this page, 1-1000 (server default 1000)"),
			mcp.Min(1), mcp.Max(protocol.MaxListPageSize), mcp.MultipleOf(1),
		),
	)
}

func markGraphTool(host string) mcp.Tool {
	return mcp.NewTool("mark_graph",
		mcp.WithDescription(
			"Crawl outbound links from a document and return the link graph. "+
				"Follows mark:// links up to the specified depth. External links are "+
				"recorded but not followed. Use this to understand document relationships "+
				"or find broken links. Edges carry provenance (link label, source section "+
				"anchor, occurrence count) and typed relations ingested from rel-<predicate> "+
				"publisher metadata. When a local graph store is available, results are "+
				"persisted for backlink queries; the store is seeded from the world's "+
				"published /graph.md when available, and local crawls take precedence. "+
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

func markVersionsTool(host string) mcp.Tool {
	return mcp.NewTool("mark_versions",
		mcp.WithDescription(
			"Retrieve the version history of a document from a Mark Protocol server. "+
				"Returns total and current version numbers, hash chain validation status, "+
				"and a list of all versions with timestamps. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
	)
}

func markLookupTool(host string) mcp.Tool {
	return mcp.NewTool("mark_lookup",
		mcp.WithDescription(
			"Look up documents by subject against a Mark Protocol server's catalog. "+
				"Matches the query against each document's declared tags and title and returns "+
				"an importance-ranked markdown table of matches (path, importance, title, tags), "+
				"not document bodies; FETCH the ones you want. This is a catalog lookup, not "+
				"full-text search: a subject that was never tagged or titled will not be found. "+
				"Optionally narrow with a comma-separated key=value filter and cap results with limit. "+
				"Start here when hunting a subject: lookup to find candidate documents, "+
				"mark_explore the best match to orient, then mark_fetch url#<anchor> for the "+
				"sections you actually need; full bodies of large documents are rarely necessary. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("scope to search under: / for everything, or a subtree like /docs/. "+urlDesc(host)),
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("subject to look up; matched against document tags and titles (minimum 2 characters), or '*' to match every catalogued document under the scope (importance order)"),
		),
		mcp.WithString("filter",
			mcp.Description("comma-separated key=value predicates applied before ranking; built-ins: tag=, modified-after=, modified-before="),
		),
		mcp.WithNumber("limit",
			mcp.Description("maximum number of results (server default 10, hard cap 1000)"),
		),
	)
}

func markPublishTool(host string) mcp.Tool {
	return mcp.NewTool("mark_publish",
		mcp.WithDescription(
			"Publish or update a document on a Mark Protocol server. "+
				"Returns the created version number and modified timestamp. "+
				"Requires an auth token configured via the -token flag. "+
				"The body should be valid markdown content. "+
				"expected_version is required for optimistic concurrency: set it to the version "+
				"number from a prior fetch to detect conflicts. If the document has been "+
				"modified since that version, the server returns a conflict status. "+
				"Use 0 when creating a new document. "+
				"on_conflict controls behavior when the version check fails. Default \"merge\" "+
				"returns a structurally-merged candidate body (with git-style conflict markers "+
				"if both sides edited the same lines) for the agent to semantically verify, "+
				"then call mark_publish again with expected_version set to the returned "+
				"publish-at-version. This prevents the silent content loss that happens when "+
				"callers naively republish their stale body. Pass \"fail\" to opt out and get "+
				"the strict conflict response with no merge attempt. "+
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
		mcp.WithNumber("expected_version",
			mcp.Required(),
			mcp.Description("version number from a prior fetch for conflict detection; use 0 when creating a new document"),
		),
		mcp.WithString("on_conflict",
			mcp.Description("conflict behavior: \"merge\" (default) returns a merge-candidate body; the agent reviews it (resolving any conflict markers) and calls mark_publish again with expected_version set to the returned publish-at-version. \"fail\" opts out and returns the raw conflict status."),
		),
		mcp.WithObject("metadata",
			mcp.Description("optional publisher metadata stored with the document, as string values. The server interprets `tags` (comma-separated subject labels) and `importance` (0-1) for mark_lookup ranking; other keys are stored opaquely. Keys with the `rel-` prefix declare typed relations to other documents (e.g. `rel-supersedes: /adr/0002.md`, comma-separated for multiple targets); graph crawls ingest them as typed edges. Reserved keys are rejected. `retention` (positive integer) caps version history: this write and every later write carrying the key permanently delete versions older than the newest N. Destructive and irreversible: confirm with the user before setting it on a document whose history matters; intended for generated documents."),
		),
	)
}

func markArchiveTool(host string) mcp.Tool {
	return mcp.NewTool("mark_archive",
		mcp.WithDescription(
			"Archive a document on a Mark Protocol server. "+
				"Returns the archived version number. "+
				"Archived documents return 'archived' status on FETCH but version history is preserved. "+
				"Requires an auth token configured via the -token flag. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
	)
}

func markAppendTool(host string) mcp.Tool {
	return mcp.NewTool("mark_append",
		mcp.WithDescription(
			"Append content to the end of an existing document on a Mark Protocol server. "+
				"The server concatenates the new content after the existing document body. "+
				"Returns the created version number and modified timestamp. "+
				"Requires an auth token configured via the -token flag. "+
				"The body should be valid markdown content to append. "+
				"expected_version is optional: when omitted or 0, the tool calls VERSIONS to get the "+
				"current version automatically. Set it explicitly if you already know the version "+
				"from a prior fetch. "+
				"The document keeps its catalog metadata across an append: the server carries the "+
				"current version's tags, importance, title, and type onto the new version, so an "+
				"append never drops a document out of mark_lookup. Use mark_publish to change or "+
				"remove metadata. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("markdown content to append"),
		),
		mcp.WithNumber("expected_version",
			mcp.Description("version number from a prior fetch for conflict detection; when omitted or 0, resolved via VERSIONS"),
		),
	)
}

func markDiscoverTool(host string) mcp.Tool {
	return mcp.NewTool("mark_discover",
		mcp.WithDescription(
			"Fetch the agent manifest from a Mark Protocol server. "+
				"Returns the manifest at /.well-known/agent-manifest.md which describes "+
				"the server's purpose, key paths, auth requirements, and usage guidelines. "+
				"Returns not-found if no manifest is published. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Description("mark:// URL of the server to discover (optional when -host is set)"),
		),
	)
}

func markResolveTool(host string) mcp.Tool {
	return mcp.NewTool("mark_resolve",
		mcp.WithDescription(
			"Resolve content by its SHA-256 hash using a hub index document. "+
				"Looks up the hash in the index, finds servers hosting the content, "+
				"and fetches it by hash. Returns the document content if found. "+
				urlHint(host),
		),
		mcp.WithString("hash",
			mcp.Required(),
			mcp.Description("content hash to resolve (sha256-<64 lowercase hex characters>)"),
		),
		mcp.WithString("index",
			mcp.Required(),
			mcp.Description("mark:// URL of the hash index document on a hub, or "+urlDesc(host)),
		),
	)
}

func markIndexTool(host string) mcp.Tool {
	return mcp.NewTool("mark_index",
		mcp.WithDescription(
			"Crawl a Mark Protocol server, collect content hashes from all documents, "+
				"and publish a hash index document to a target server (typically a hub). "+
				"The tool checks the target's agent manifest before publishing. "+
				urlHint(host),
		),
		mcp.WithString("source",
			mcp.Required(),
			mcp.Description("mark:// URL of the server to crawl, or "+urlDesc(host)),
		),
		mcp.WithString("target",
			mcp.Required(),
			mcp.Description("mark:// URL where the index should be published, or "+urlDesc(host)),
		),
		mcp.WithNumber("expected_version",
			mcp.Description("version of existing index at target for conflict detection; use 0 to create new"),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("if true, return the index document without publishing (default false)"),
		),
		mcp.WithBoolean("force",
			mcp.Description("if true, publish even when target has no hub manifest (default false)"),
		),
	)
}

// formatResult builds a text response with status, selected metadata keys, and body.
// After the explicitly requested keys, any remaining metadata keys (e.g. publisher
// metadata) are appended in sorted order so output is deterministic across calls —
// Go's map iteration is randomized, and the broker MCP gateway's mirror of this
// formatter relies on byte-for-byte agreement with this function to satisfy the
// proxy-fidelity contract documented in /plans/broker-https-gateway.md.
func formatResult(r fetch.Result, keys ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", r.Response.Status)
	shown := make(map[string]bool, len(keys))
	for _, key := range keys {
		if v, ok := r.Response.Metadata[key]; ok {
			fmt.Fprintf(&b, "%s: %s\n", key, v)
			shown[key] = true
		}
	}
	remaining := make([]string, 0, len(r.Response.Metadata))
	for k := range r.Response.Metadata {
		if !shown[k] {
			remaining = append(remaining, k)
		}
	}
	sort.Strings(remaining)
	for _, k := range remaining {
		fmt.Fprintf(&b, "%s: %s\n", k, r.Response.Metadata[k])
	}
	if r.Response.Body != "" {
		b.WriteString("\n")
		b.WriteString(r.Response.Body)
	}
	return b.String()
}

// formatResultWith renders r via formatResult with a replacement body and
// extra metadata entries. The metadata map is cloned first — r may hold a
// cached response whose map must never be mutated.
func formatResultWith(r fetch.Result, body string, extra map[string]string, keys ...string) string {
	meta := maps.Clone(r.Response.Metadata)
	if meta == nil {
		meta = make(map[string]string, len(extra))
	}
	maps.Copy(meta, extra)
	r.Response.Metadata = meta
	r.Response.Body = body
	return formatResult(r, keys...)
}

// agentMeta returns publisher metadata with the "agent" key set to the MCP
// client name from the session context. If the client name is unavailable,
// it falls back to "unknown".
// publisherMeta merges caller-supplied publisher metadata (the optional
// "metadata" object argument, values coerced to strings) with the agent
// identity. It starts from the agent map and skips a caller-supplied "agent"
// key so identity cannot be spoofed. The server validates keys/values and
// rejects reserved keys, so this stays a thin pass-through.
func publisherMeta(ctx context.Context, args map[string]any) map[string]string {
	meta := agentMeta(ctx)
	raw, ok := args["metadata"].(map[string]any)
	if !ok {
		return meta
	}
	for k, v := range raw {
		if k == "agent" {
			continue // identity is server-set; callers cannot override it
		}
		meta[k] = fmt.Sprintf("%v", v)
	}
	return meta
}

func agentMeta(ctx context.Context) map[string]string {
	name := "unknown"
	if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
		if s, ok := session.(mcpserver.SessionWithClientInfo); ok {
			if n := s.GetClientInfo().Name; n != "" {
				name = n
			}
		}
	}
	return map[string]string{"agent": name}
}

// Tool handlers.
// Handler signatures are dictated by mcp-go's ToolHandlerFunc type.

func (h *handler) markFetch(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	docURL, anchor, _ := strings.Cut(rawURL, "#")
	force := req.GetBool("force", false)

	host, path, err := h.resolveURL(docURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	result, err := h.client.Fetch(host, path, h.resolveToken(host))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch failed: %v", err)), nil
	}
	if result.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultText(formatResult(result, "version", "modified", "etag")), nil
	}

	body := result.Response.Body
	version := result.Response.Metadata["version"]
	etag := result.Response.Metadata["etag"]
	key := host + path

	// Binary/non-UTF-8 body: always a notice, never bytes. MCP text can't carry
	// binary faithfully (JSON mangles it); byte-exact retrieval is the CLI's job.
	if mdoutline.BinaryBody(body) {
		return mcp.NewToolResultText(formatResultWith(result, mdoutline.NonMarkdownNotice(len(body)),
			map[string]string{"mode": "binary"}, "version", "modified", "etag")), nil
	}

	// #section slice: works at any size and bypasses dedup — the agent is
	// asking for content it has not necessarily seen.
	if anchor != "" {
		section, ok := mdoutline.Section(body, anchor)
		if !ok {
			available := strings.Join(mdoutline.Anchors(body), ", ")
			if available == "" {
				available = "(document has no headings)"
			}
			return mcp.NewToolResultError(fmt.Sprintf("section #%s not found in %s; available anchors: %s", anchor, docURL, available)), nil
		}
		return mcp.NewToolResultText(formatResultWith(result, section,
			map[string]string{"section": "#" + anchor}, "version", "modified", "etag")), nil
	}

	// Dedup needs at least one identity field: with version and etag both
	// absent, two different bodies would compare equal and a changed
	// document would be silently reported as unchanged.
	cur := fetchdedup.Doc{Version: version, Etag: etag}
	prev, seenBefore := h.seenLookup(key)
	if seenBefore && !force && cur.Identified() && prev == cur {
		return mcp.NewToolResultText(fetchdedup.UnchangedNotice(cur)), nil
	}

	extra := map[string]string{}
	// cur.Identified() also gates the note: a response that lost both
	// identity fields has nothing truthful to say about what changed.
	if seenBefore && cur.Identified() && prev != cur {
		extra["note"] = fetchdedup.ChangedNote(prev, cur)
	}

	// Size gate: large documents return an outline unless forced.
	if !force && len(body) >= outlineThreshold {
		extra["mode"] = "outline"
		extra["size"] = fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1)
		return mcp.NewToolResultText(formatResultWith(result, mdoutline.OutlineBody(docURL, body),
			extra, "version", "modified", "etag")), nil
	}

	if cur.Identified() {
		h.seenRecord(key, cur)
	}
	if len(extra) == 0 {
		return mcp.NewToolResultText(formatResult(result, "version", "modified", "etag")), nil
	}
	return mcp.NewToolResultText(formatResultWith(result, body, extra, "version", "modified", "etag")), nil
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

	pageSize, err := mcpListPageSize(&req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	opts := fetch.ListOptions{
		IncludeArchived: req.GetBool("include_archived", false),
		Cursor:          req.GetString("cursor", ""),
		PageSize:        pageSize,
	}
	result, err := h.client.ListWithOptions(host, path, h.resolveToken(host), opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list failed: %v", err)), nil
	}
	if result.Response.Metadata["complete"] == "false" {
		next := result.Response.Metadata["next-cursor"]
		if next == "" || next == opts.Cursor {
			return mcp.NewToolResultError("list failed: continuation cursor is missing or did not advance"), nil
		}
	}

	return mcp.NewToolResultText(formatResult(result, "modified")), nil
}

func mcpListPageSize(req *mcp.CallToolRequest) (int, error) {
	raw, ok := req.GetArguments()["page_size"]
	if !ok {
		return 0, nil
	}
	var size int
	switch value := raw.(type) {
	case int:
		size = value
	case float64:
		if value != math.Trunc(value) {
			return 0, errors.New("page_size must be an integer")
		}
		size = int(value)
	default:
		return 0, errors.New("page_size must be an integer")
	}
	if size < 1 || size > protocol.MaxListPageSize {
		return 0, fmt.Errorf("page_size must be between 1 and %d", protocol.MaxListPageSize)
	}
	return size, nil
}

func (h *handler) markVersions(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	result, err := h.client.Versions(host, path, h.resolveToken(host))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("versions failed: %v", err)), nil
	}

	return mcp.NewToolResultText(formatResult(result, "total", "current", "chain-valid", "chain-error")), nil
}

func (h *handler) markLookup(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}

	host, scope, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	opts := fetch.LookupOptions{
		Filter: req.GetString("filter", ""),
		Limit:  req.GetInt("limit", 0),
	}
	result, err := h.client.Lookup(host, scope, query, h.resolveToken(host), opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("lookup failed: %v", err)), nil
	}

	return mcp.NewToolResultText(formatResult(result, "matches")), nil
}

func (h *handler) markPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
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

	token := h.resolveToken(host)
	if token == "" {
		return mcp.NewToolResultError("publish requires a token (-token flag, DEMARKUS_AUTH env var, or stored via 'demarkus token add')"), nil
	}

	expectedVersion, err := req.RequireInt("expected_version")
	if err != nil {
		return mcp.NewToolResultError("expected_version is required"), nil
	}
	// Validate before the on_conflict switch so both branches (merge and
	// fail) reject invalid input the same way and surface a clear local
	// error rather than forwarding it to the server.
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be >= 0"), nil
	}

	// Default is "merge": on conflict, return a structurally-merged candidate
	// for the agent to verify and republish. Callers wanting the strict
	// optimistic-concurrency behavior (a hard conflict response with no merge
	// attempt) opt out with on_conflict: "fail". Default flipped because the
	// whole point of the feature is preventing content loss; "fail" left as
	// default would mean naive callers never benefit. An explicit empty or
	// whitespace-only value is normalized to the default — MCP clients
	// commonly send blank strings for optional fields, and rejecting them
	// would turn the default path into a needless tool error.
	onConflict := strings.TrimSpace(req.GetString("on_conflict", "merge"))
	if onConflict == "" {
		onConflict = "merge"
	}
	switch onConflict {
	case "fail":
		// fall through to plain publish
	case "merge":
		adapter := &mergeClientAdapter{inner: h.client, host: host, token: token}
		outcome, mErr := merge.Candidate(adapter, path, body, expectedVersion, publisherMeta(ctx, req.GetArguments()))
		if mErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", mErr)), nil
		}
		return mcp.NewToolResultText(formatOutcome(&outcome)), nil
	default:
		return mcp.NewToolResultError(fmt.Sprintf("invalid on_conflict %q: expected \"fail\" or \"merge\"", onConflict)), nil
	}

	result, err := h.client.Publish(host, path, body, token, expectedVersion, publisherMeta(ctx, req.GetArguments()))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", err)), nil
	}

	return mcp.NewToolResultText(formatResult(result, "version", "modified", "server-version")), nil
}

// mergeClientAdapter exposes the markClient interface as a merge.Client. It
// turns versioned fetches into the path/vN suffix the server understands and
// extracts version metadata from the protocol response.
type mergeClientAdapter struct {
	inner markClient
	host  string
	token string
}

// FetchVersion fetches a specific historical version via the /path/vN route.
func (a *mergeClientAdapter) FetchVersion(path string, version int) (merge.Doc, error) {
	versionedPath := strings.TrimRight(path, "/") + "/v" + strconv.Itoa(version)
	r, err := a.inner.Fetch(a.host, versionedPath, a.token)
	if err != nil {
		return merge.Doc{}, err
	}
	return docFromResponse(r)
}

// FetchCurrent fetches the current head version of path.
func (a *mergeClientAdapter) FetchCurrent(path string) (merge.Doc, error) {
	r, err := a.inner.Fetch(a.host, path, a.token)
	if err != nil {
		return merge.Doc{}, err
	}
	return docFromResponse(r)
}

// Publish forwards to the underlying client and lifts the protocol response
// into a merge.PublishResult, parsing version and server-version metadata.
func (a *mergeClientAdapter) Publish(path, body string, expectedVersion int, meta map[string]string) (merge.PublishResult, error) {
	r, err := a.inner.Publish(a.host, path, body, a.token, expectedVersion, meta)
	if err != nil {
		return merge.PublishResult{}, err
	}
	v, err := optionalInt(r.Response.Metadata, "version")
	if err != nil {
		return merge.PublishResult{}, err
	}
	sv, err := optionalInt(r.Response.Metadata, "server-version")
	if err != nil {
		return merge.PublishResult{}, err
	}
	return merge.PublishResult{
		Status:        r.Response.Status,
		Version:       v,
		ServerVersion: sv,
		Metadata:      r.Response.Metadata,
	}, nil
}

// docFromResponse converts a fetch.Result into the merge package's Doc type,
// extracting the integer version from response metadata.
func docFromResponse(r fetch.Result) (merge.Doc, error) {
	v, err := optionalInt(r.Response.Metadata, "version")
	if err != nil {
		return merge.Doc{}, err
	}
	return merge.Doc{
		Status:  r.Response.Status,
		Body:    r.Response.Body,
		Version: v,
	}, nil
}

// optionalInt parses metadata[key] as an int. A missing or empty value
// returns 0 with no error (the natural sentinel for absent versions). A
// present but malformed value returns a wrapped parse error so the caller
// surfaces server-side corruption rather than silently treating it as 0.
func optionalInt(meta map[string]string, key string) (int, error) {
	s, ok := meta[key]
	if !ok || s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("response metadata %q = %q: %w", key, s, err)
	}
	return n, nil
}

// formatOutcome renders a merge.Candidate outcome in the same key:value text
// shape as formatResult. On OutcomeOK it delegates to formatResult so the
// success response is byte-for-byte identical to a plain mark_publish — the
// on_conflict=merge opt-in does not change the success contract. On
// OutcomeCandidate it surfaces merge metadata followed by the candidate body
// (with or without conflict markers) after a blank line.
func formatOutcome(o *merge.Outcome) string {
	switch o.Status {
	case merge.OutcomeOK:
		return formatResult(fetch.Result{
			Response: protocol.Response{
				Status:   o.Publish.Status,
				Metadata: o.Publish.Metadata,
			},
		}, "version", "modified", "server-version")
	case merge.OutcomeCandidate:
		var b strings.Builder
		b.WriteString("status: merge-candidate\n")
		fmt.Fprintf(&b, "your-version: %d\n", o.BaseVersion)
		fmt.Fprintf(&b, "current-version: %d\n", o.TheirVersion)
		fmt.Fprintf(&b, "publish-at-version: %d\n", o.PublishAtVersion)
		fmt.Fprintf(&b, "has-markers: %t\n", o.HasMarkers)
		b.WriteString("\n")
		b.WriteString(o.Body)
		return b.String()
	}
	return ""
}

func (h *handler) markArchive(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	token := h.resolveToken(host)
	if token == "" {
		return mcp.NewToolResultError("archive requires a token (-token flag, DEMARKUS_AUTH env var, or stored via 'demarkus token add')"), nil
	}

	result, err := h.client.Archive(host, path, token)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("archive failed: %v", err)), nil
	}

	return mcp.NewToolResultText(formatResult(result, "version")), nil
}

func (h *handler) markAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
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

	token := h.resolveToken(host)
	if token == "" {
		return mcp.NewToolResultError("append requires a token (-token flag, DEMARKUS_AUTH env var, or stored via 'demarkus token add')"), nil
	}

	expectedVersion := req.GetInt("expected_version", 0)
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be >= 0"), nil
	}
	if expectedVersion == 0 {
		// Auto-resolve via VERSIONS.
		vResult, vErr := h.client.Versions(host, path, h.resolveToken(host))
		if vErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not resolve version: %v", vErr)), nil
		}
		if vResult.Response.Status != protocol.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("could not resolve version: %s", vResult.Response.Status)), nil
		}
		cur, ok := vResult.Response.Metadata["current"]
		if !ok {
			return mcp.NewToolResultError("could not resolve version: no current version in response"), nil
		}
		expectedVersion, err = strconv.Atoi(cur)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not resolve version: invalid current version %q", cur)), nil
		}
	}

	result, err := h.client.Append(host, path, body, token, expectedVersion, agentMeta(ctx))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append failed: %v", err)), nil
	}

	return mcp.NewToolResultText(formatResult(result, "version", "modified", "server-version")), nil
}

func (h *handler) markDiscover(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, _ := req.RequireString("url")

	var host, path string
	var err error
	if rawURL != "" {
		host, _, err = h.resolveURL(rawURL)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
		}
		path = protocol.WellKnownManifestPath
	} else {
		if h.defaultHost == "" {
			return mcp.NewToolResultError("no server specified: provide a URL or set -host"), nil
		}
		host, path, err = fetch.ParseMarkURL(h.defaultHost + protocol.WellKnownManifestPath)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid host: %v", err)), nil
		}
	}

	result, err := h.client.Fetch(host, path, "")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("discover failed: %v", err)), nil
	}

	return mcp.NewToolResultText(formatResult(result, "version", "modified")), nil
}

func (h *handler) markResolve(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	hash, err := req.RequireString("hash")
	if err != nil {
		return mcp.NewToolResultError("hash is required"), nil
	}
	if _, ok := protocol.IsHashPath(hash); !ok {
		return mcp.NewToolResultError("invalid hash format: expected sha256-<64 lowercase hex characters>"), nil
	}

	indexURL, err := req.RequireString("index")
	if err != nil {
		return mcp.NewToolResultError("index is required"), nil
	}

	indexHost, indexPath, err := h.resolveURL(indexURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid index URL: %v", err)), nil
	}

	// Fetch the index document.
	indexResult, err := h.client.Fetch(indexHost, indexPath, h.resolveToken(indexHost))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to fetch index: %v", err)), nil
	}
	if indexResult.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("index fetch returned: %s", indexResult.Response.Status)), nil
	}

	// Parse index and filter for matching hash.
	entries := index.Parse(indexResult.Response.Body)
	var matches []index.Entry
	for _, e := range entries {
		if e.Hash == hash {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("hash %s not found in index", hash)), nil
	}

	// Try each matching server.
	var lastErr string
	for _, m := range matches {
		serverHost, _, err := fetch.ParseMarkURL(m.Server + "/")
		if err != nil {
			lastErr = fmt.Sprintf("invalid server URL %s: %v", m.Server, err)
			continue
		}
		result, err := h.client.Fetch(serverHost, "/"+hash, h.resolveToken(serverHost))
		if err != nil {
			lastErr = fmt.Sprintf("%s: %v", m.Server, err)
			continue
		}
		if result.Response.Status != protocol.StatusOK {
			lastErr = fmt.Sprintf("%s: %s", m.Server, result.Response.Status)
			continue
		}
		// Verify content hash matches.
		if got := result.Response.Metadata["content-hash"]; got != hash {
			lastErr = fmt.Sprintf("%s: hash mismatch (got %s)", m.Server, got)
			continue
		}
		return mcp.NewToolResultText(formatResult(result, "version", "modified", "content-hash")), nil
	}

	return mcp.NewToolResultError(fmt.Sprintf("could not resolve hash from any server: %s", lastErr)), nil
}

const maxIndexDocuments = 1000

// errIndexTruncated is returned by walkDir when the document limit is reached.
var errIndexTruncated = errors.New("document limit reached, index is truncated")

// checkManifests verifies agent manifests on source and target servers.
// Returns warnings, a tool error result (if blocked), or nil to proceed.
func (h *handler) checkManifests(sourceHost, targetHost string, dryRun, force bool) (warnings []string, block *mcp.CallToolResult) {
	// Check source manifest (warn only).
	srcManifest, err := h.client.Fetch(sourceHost, protocol.WellKnownManifestPath, "")
	if err != nil || srcManifest.Response.Status != protocol.StatusOK {
		warnings = append(warnings, "warning: source server has no agent manifest")
	}

	// Check target manifest (block unless force or dry run).
	if !dryRun {
		tgtManifest, err := h.client.Fetch(targetHost, protocol.WellKnownManifestPath, "")
		if err != nil || tgtManifest.Response.Status != protocol.StatusOK {
			if !force {
				return warnings, mcp.NewToolResultError(
					"target server has no agent manifest; cannot verify it accepts index publications. " +
						"Use force=true to override, or publish a manifest at /.well-known/agent-manifest.md on the target.",
				)
			}
			warnings = append(warnings, "warning: target server has no agent manifest (force=true override)")
		}
	}

	return warnings, nil
}

func (h *handler) markIndex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	sourceURL, err := req.RequireString("source")
	if err != nil {
		return mcp.NewToolResultError("source is required"), nil
	}
	targetURL, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError("target is required"), nil
	}

	sourceHost, sourcePath, err := h.resolveURL(sourceURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid source URL: %v", err)), nil
	}
	if sourcePath == "" {
		sourcePath = "/"
	}
	targetHost, targetPath, err := h.resolveURL(targetURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid target URL: %v", err)), nil
	}

	dryRun := req.GetBool("dry_run", false)
	force := req.GetBool("force", false)
	expectedVersion := req.GetInt("expected_version", 0)
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be non-negative"), nil
	}

	warnings, block := h.checkManifests(sourceHost, targetHost, dryRun, force)
	if block != nil {
		return block, nil
	}

	// Crawl source server.
	sourceScheme := "mark://" + sourceHost
	entries, crawlWarnings, err := h.collectEntries(ctx, sourceHost, sourcePath, h.resolveToken(sourceHost))
	warnings = append(warnings, crawlWarnings...)
	if err != nil && !errors.Is(err, errIndexTruncated) {
		return mcp.NewToolResultError(fmt.Sprintf("crawl failed: %v", err)), nil
	} else if errors.Is(err, errIndexTruncated) {
		warnings = append(warnings, fmt.Sprintf("warning: index truncated at %d documents, some content may not be indexed", maxIndexDocuments))
	}

	body := index.Build(sourceScheme, timeNow(), entries)

	if dryRun {
		var b strings.Builder
		for _, w := range warnings {
			b.WriteString(w + "\n")
		}
		fmt.Fprintf(&b, "Indexed %d documents from %s (dry run, not published)\n\n", len(entries), sourceScheme)
		b.WriteString(body)
		return mcp.NewToolResultText(b.String()), nil
	}
	if errors.Is(err, errIndexTruncated) || len(crawlWarnings) > 0 {
		return mcp.NewToolResultError("crawl incomplete; refusing to publish an authoritative index"), nil
	}

	token := h.resolveToken(targetHost)
	if token == "" {
		return mcp.NewToolResultError("publishing requires a token (-token flag, DEMARKUS_AUTH env var, or stored via 'demarkus token add')"), nil
	}

	// Merge with existing index if updating.
	if expectedVersion > 0 {
		existing, err := h.client.Fetch(targetHost, targetPath, h.resolveToken(targetHost))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to fetch existing index: %v", err)), nil
		}
		if existing.Response.Status != protocol.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("failed to fetch existing index: %s", existing.Response.Status)), nil
		}
		existingEntries := index.Parse(existing.Response.Body)
		merged := index.Merge(existingEntries, sourceScheme, entries)
		// Use the target as source header since this is now an aggregated index.
		targetScheme := "mark://" + targetHost
		body = index.Build(targetScheme, timeNow(), merged)
	}

	result, err := h.client.Publish(targetHost, targetPath, body, token, expectedVersion, agentMeta(ctx))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", err)), nil
	}

	var b strings.Builder
	for _, w := range warnings {
		b.WriteString(w + "\n")
	}
	fmt.Fprintf(&b, "Indexed %d documents from %s\n", len(entries), sourceScheme)
	b.WriteString(formatResult(result, "version", "modified"))
	return mcp.NewToolResultText(b.String()), nil
}

// collectEntries walks the listings and fetches each document, collecting
// content-hash index entries. Skips come back as warnings (a partial index is
// never a silent success); errIndexTruncated caps at maxIndexDocuments.
func (h *handler) collectEntries(ctx context.Context, host, dirPath, token string) ([]index.Entry, []string, error) {
	var entries []index.Entry
	var warnings []string
	attempts := 0
	w := listwalk.Walker{
		Client: h.client,
		Host:   host,
		Token:  token,
		OnSkip: func(p, reason string) {
			warnings = append(warnings, fmt.Sprintf("warning: skipped %s: %s", p, reason))
		},
	}
	err := w.Walk(dirPath, func(fullPath string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempts >= maxIndexDocuments {
			return errIndexTruncated
		}
		attempts++

		// Fetch and collect content-hash.
		doc, err := h.client.Fetch(host, fullPath, token)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("warning: skipped %s: %v", fullPath, err))
			return nil
		}
		if doc.Response.Status != protocol.StatusOK {
			warnings = append(warnings, fmt.Sprintf("warning: skipped %s: %s", fullPath, doc.Response.Status))
			return nil
		}
		contentHash, ok := doc.Response.Metadata["content-hash"]
		if _, valid := protocol.IsHashPath(contentHash); !ok || !valid {
			warnings = append(warnings, fmt.Sprintf("warning: skipped %s: missing or invalid content-hash", fullPath))
			return nil
		}
		entries = append(entries, index.Entry{
			Hash:   contentHash,
			Server: "mark://" + host,
			Path:   fullPath,
		})
		return nil
	})
	if errors.Is(err, listwalk.ErrListBudget) {
		warnings = append(warnings, "warning: directory budget exhausted, index is incomplete")
		err = nil
	}
	return entries, warnings, err
}

// timeNow is a variable for testing.
var timeNow = time.Now

func (h *handler) markGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	depth := max(1, min(req.GetInt("depth", 2), 5))

	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	// Node identity omits the default port (ADR 0005), so crawled rows share
	// the key form of the hub's /graph.md aggregate and backlink lookups.
	startURL := links.NodeURL(host, path)

	if h.graphStore == nil {
		return mcp.NewToolResultError("graph store not available"), nil
	}

	// Seed before crawling so depth-limited crawls still benefit from hub context.
	h.seedGraph(host)

	g, err := h.graphStore.CrawlAndPersist(ctx, startURL, func(host, path string) (graph.FetchResult, error) {
		r, fetchErr := h.client.Fetch(host, path, h.resolveToken(host))
		if fetchErr != nil {
			return graph.FetchResult{}, fetchErr
		}
		return graph.FetchResult{
			Status:   r.Response.Status,
			Body:     r.Response.Body,
			Metadata: r.Response.Metadata,
		}, nil
	}, fetch.ParseMarkURL, graphstore.CrawlOptions{
		MaxDepth: depth,
		MaxNodes: 200,
		Workers:  5,
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
			fmt.Fprintf(&b, "  %s -> %s%s\n", e.From, e.To, e.Annotation())
		}
	}

	return b.String()
}

func markBacklinksTool(host string) mcp.Tool {
	return mcp.NewTool("mark_backlinks",
		mcp.WithDescription(
			"Look up which documents link to a given URL, using the local graph store. "+
				"Returns results from previous crawls; run mark_graph first to populate. "+
				"The store is seeded from the world's published /graph.md when available, "+
				"so backlinks answer even before any local crawl; local crawls take precedence. "+
				"Each backlink shows its provenance (link label, source section anchor, "+
				"occurrence count) and typed relations like [supersedes] when present. "+
				"mark_explore includes this same backlink list alongside the document's "+
				"outline, outbound links, and siblings; prefer it for general orientation. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
	)
}

func (h *handler) markBacklinks(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	// Key on node identity, which omits the default port (ADR 0005), so the
	// lookup matches both crawled and seeded rows.
	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	fullURL := links.NodeURL(host, path)

	if h.graphStore == nil {
		return mcp.NewToolResultError("graph store not available"), nil
	}

	h.seedGraph(host)

	backlinks := h.graphStore.BacklinksEnriched(fullURL)
	if len(backlinks) == 0 {
		return mcp.NewToolResultText(
			fmt.Sprintf("No backlinks found for %s\nRun mark_graph to populate the graph store.", fullURL),
		), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Backlinks for %s (%d):\n\n", fullURL, len(backlinks))
	for _, bl := range backlinks {
		ann := graph.EdgeAnnotation(bl.Rel, bl.Label, bl.Anchor, bl.Count)
		if bl.Title != "" {
			fmt.Fprintf(&b, "- [%s](%s)%s\n", bl.Title, bl.URL, ann)
		} else {
			fmt.Fprintf(&b, "- %s%s\n", bl.URL, ann)
		}
	}
	return mcp.NewToolResultText(b.String()), nil
}

func markGraphExportTool() mcp.Tool {
	return mcp.NewTool("mark_graph_export",
		mcp.WithDescription(
			"Export the local graph store as a publishable markdown document. "+
				"The output contains mark:// links so crawling it naturally discovers the topology. "+
				"Run mark_graph first to populate the store.",
		),
	)
}

// defaultGraphRetention bounds the published graph document's version history.
// The graph is a generated artifact republished wholesale on every run, so
// unbounded history is pure growth (one live document reached 545 versions);
// 20 versions is enough to debug a bad crawl.
const defaultGraphRetention = 20

func markGraphPublishTool(host string) mcp.Tool {
	return mcp.NewTool("mark_graph_publish",
		mcp.WithDescription(
			"Export the local graph store and publish it to a Mark Protocol server. "+
				"Combines mark_graph_export + mark_publish in one step. "+
				"The published document contains mark:// links so other agents can crawl it "+
				"to discover the topology without recrawling the original servers. "+
				"Run mark_graph first to populate the store. "+
				"Requires an auth token configured via the -token flag. "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("target URL where the graph document should be published, e.g. /graph.md or "+urlDesc(host)),
		),
		mcp.WithNumber("expected_version",
			mcp.Required(),
			mcp.Description("version number from a prior fetch for conflict detection; use 0 when creating a new document"),
		),
		mcp.WithNumber("retention",
			mcp.Description("how many versions of the graph document to keep (default 20). The graph is regenerated on every publish, so older versions are permanently pruned. Pass 0 to keep every version."),
		),
	)
}

func (h *handler) markGraphExport(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	if h.graphStore == nil {
		return mcp.NewToolResultError("graph store not available"), nil
	}

	md := h.graphStore.Export()
	return mcp.NewToolResultText(md), nil
}

func (h *handler) markGraphPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	if h.graphStore == nil {
		return mcp.NewToolResultError("graph store not available"), nil
	}

	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}

	host, path, err := h.resolveURL(rawURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	token := h.resolveToken(host)
	if token == "" {
		return mcp.NewToolResultError("publish requires a token (-token flag, DEMARKUS_AUTH env var, or stored via 'demarkus token add')"), nil
	}

	expectedVersion, err := req.RequireInt("expected_version")
	if err != nil {
		return mcp.NewToolResultError("expected_version is required"), nil
	}
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be >= 0"), nil
	}

	retention := req.GetInt("retention", defaultGraphRetention)
	if retention < 0 {
		return mcp.NewToolResultError("retention must be >= 0 (0 keeps every version)"), nil
	}

	md := h.graphStore.Export()

	meta := agentMeta(ctx)
	if retention > 0 {
		meta["retention"] = strconv.Itoa(retention)
	}
	result, err := h.client.Publish(host, path, md, token, expectedVersion, meta)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Published graph (%d nodes, %d edges) to mark://%s%s\n",
		h.graphStore.NodeCount(), h.graphStore.EdgeCount(), host, path)
	b.WriteString(formatResult(result, "version", "modified", "server-version"))
	return mcp.NewToolResultText(b.String()), nil
}
