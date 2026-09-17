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
	"math"
	"net"
	"os"
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
	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/client/metaguard"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	defaultHost := flag.String("host", "", "default Mark server (e.g. mark://localhost:6309)")
	dialAddress := flag.String("dial-address", "", "network address for the default host; preserves logical URL identity")
	token := flag.String("token", "", "auth token for capability-based authentication")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	noCache := flag.Bool("no-cache", false, "disable response caching")
	cacheDir := flag.String("cache-dir", cache.DefaultDir(), "cache directory")
	profile := flag.String("profile", profileDefault(), "tool profile: lean (plugin surface) or full; default from DEMARKUS_MCP_PROFILE")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := mcpfmt.ValidProfile(*profile); err != nil {
		log.Fatal(err)
	}

	opts := fetch.Options{Insecure: *insecure}
	if err := routeDefaultHost(&opts, *defaultHost, *dialAddress); err != nil {
		log.Fatal(err)
	}
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
		mcpserver.WithInstructions(hostInstructions(*defaultHost)+" "+mcpfmt.SectionFirst+" "+mcpfmt.ReadOutcomes),
	)

	gs, gsErr := graphstore.Load(graphstore.DefaultPath())
	if gsErr != nil {
		log.Printf("warning: graph store unavailable: %v", gsErr)
	}
	h := &handler{client: client, defaultHost: *defaultHost, token: *token, graphStore: gs}
	s.AddTools(h.profileTools(*defaultHost, *profile)...)

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

func routeDefaultHost(opts *fetch.Options, defaultHost, dialAddress string) error {
	if dialAddress == "" {
		return nil
	}
	host, _, err := fetch.ParseMarkURL(defaultHost)
	if err != nil {
		return fmt.Errorf("dial-address requires a valid default host: %w", err)
	}
	name, port, err := net.SplitHostPort(dialAddress)
	if err != nil || name == "" {
		return fmt.Errorf("invalid dial-address %q; expected host:port", dialAddress)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid dial-address port %q", port)
	}
	serverName, _, err := net.SplitHostPort(host)
	if err != nil {
		return err
	}
	opts.Endpoints = map[string]fetch.Endpoint{host: {DialAddress: dialAddress, ServerName: serverName}}
	return nil
}

// markClient defines the fetch operations used by MCP tool handlers.
type markClient interface {
	Fetch(host, path, token string) (fetch.Result, error)
	FetchContext(ctx context.Context, host, path, token string) (fetch.Result, error)
	FetchConditional(host, path, token, etag string) (fetch.Result, error)
	FetchConditionalContext(ctx context.Context, host, path, token, etag string) (fetch.Result, error)
	List(host, path, token string) (fetch.Result, error)
	ListWithOptions(host, path, token string, opts fetch.ListOptions) (fetch.Result, error)
	Versions(host, path, token string) (fetch.Result, error)
	Lookup(host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error)
	Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	PublishContext(ctx context.Context, host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
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

	// seedGate single-flights and throttles published graph checks per host.
	seedGate graphstore.SeedGate
}

// profileEnv lets a launcher select the profile without a flag an older
// binary would reject; the flag still wins when given.
const profileEnv = "DEMARKUS_MCP_PROFILE"

func profileDefault() string {
	if profile := os.Getenv(profileEnv); profile != "" {
		return profile
	}
	return mcpfmt.ProfileFull
}

// requiresToken marks writes on the stdio surface, which authenticates by flag.
const requiresToken = "Requires -token."

// tools is the full surface in registration order.
func (h *handler) tools(host string) []mcpserver.ServerTool {
	url := urlDesc(host)
	return []mcpserver.ServerTool{
		{Tool: mcpfmt.FetchTool(url, ""), Handler: h.markFetch},
		{Tool: mcpfmt.ExploreTool(url, ""), Handler: h.markExplore},
		{Tool: mcpfmt.ListTool(url, ""), Handler: h.markList},
		{Tool: mcpfmt.GraphTool(url, ""), Handler: h.markGraph},
		{Tool: mcpfmt.VersionsTool(url, ""), Handler: h.markVersions},
		{Tool: markLookupTool(host), Handler: h.markLookup},
		{Tool: mcpfmt.PublishTool(url, requiresToken), Handler: h.markPublish},
		{Tool: mcpfmt.ArchiveTool(url, requiresToken), Handler: h.markArchive},
		{Tool: mcpfmt.AppendTool(url, requiresToken), Handler: h.markAppend},
		{Tool: markDiscoverTool(), Handler: h.markDiscover},
		{Tool: markResolveTool(host), Handler: h.markResolve},
		{Tool: markIndexTool(host), Handler: h.markIndex},
		{Tool: mcpfmt.BacklinksTool(url, ""), Handler: h.markBacklinks},
		{Tool: mcpfmt.GraphExportTool(""), Handler: h.markGraphExport},
		{Tool: mcpfmt.GraphPublishTool("graph document target, e.g. /graph.md or "+url, requiresToken), Handler: h.markGraphPublish},
	}
}

// profileTools is the surface main registers; tests measure the same list.
func (h *handler) profileTools(host, profile string) []mcpserver.ServerTool {
	all := h.tools(host)
	selected := make([]mcpserver.ServerTool, 0, len(all))
	for i := range all {
		if mcpfmt.ProfileIncludes(profile, all[i].Tool.Name) {
			selected = append(selected, all[i])
		}
	}
	return selected
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

// seedGraph refreshes from the published graph so cold backlink queries work.
// Source revisions select adjacency; failures preserve last-good observations.
func (h *handler) seedGraph(ctx context.Context, host string) {
	if h.graphStore == nil || h.client == nil || host == "" {
		return
	}
	h.seedGate.Run(ctx, host, func(ctx context.Context) bool {
		if h.seedPass(ctx, host) {
			return true
		}
		h.graphStore.MarkSeedFailure(host)
		if err := h.graphStore.Save(); err != nil {
			log.Printf("warning: graph seed failure save: %v", err)
		}
		return false
	})
}

// seedPass reports whether the published graph was current, refreshed or absent.
func (h *handler) seedPass(ctx context.Context, host string) bool { //nolint:gocyclo // snapshot-first fallback has explicit terminal states
	token := h.resolveToken(host)
	etag := h.graphStore.SeedEtag(host)
	result, err := h.client.FetchConditionalContext(ctx, host, graphstore.SnapshotManifestPath, token, etag)
	if err != nil {
		log.Printf("warning: graph snapshot fetch mark://%s%s: %v", host, graphstore.SnapshotManifestPath, err)
		return false
	}
	if result.Response.Status == protocol.StatusNotModified {
		return true
	}
	if result.Response.Status == protocol.StatusOK {
		nodes, edges, loadErr := graphstore.LoadSnapshot(graphstore.SnapshotManifestPath, result.Response, func(shardPath string) (protocol.Response, error) {
			shard, fetchErr := h.client.FetchContext(ctx, host, shardPath, token)
			return shard.Response, fetchErr
		})
		if loadErr != nil {
			log.Printf("warning: graph snapshot load mark://%s%s: %v", host, graphstore.SnapshotManifestPath, loadErr)
			return false
		}
		h.graphStore.ReplaceSeed(host, nodes, edges)
		if snapshotEtag := result.Response.Metadata["etag"]; snapshotEtag != "" {
			h.graphStore.SetSeedEtag(host, snapshotEtag)
		}
		if err := h.graphStore.Save(); err != nil {
			log.Printf("warning: graph seed save: %v", err)
		}
		return true
	}
	if result.Response.Status != protocol.StatusNotFound {
		log.Printf("warning: graph snapshot mark://%s%s returned %s", host, graphstore.SnapshotManifestPath, result.Response.Status)
		return false
	}

	const legacyPath = "/graph.md"
	result, err = h.client.FetchConditionalContext(ctx, host, legacyPath, token, etag)
	if err != nil {
		log.Printf("warning: graph seed fetch mark://%s%s: %v", host, legacyPath, err)
		return false
	}
	if result.Response.Status == protocol.StatusNotFound {
		return true
	}
	if result.Response.Status == protocol.StatusNotModified {
		return true
	}
	if result.Response.Status != protocol.StatusOK {
		log.Printf("warning: legacy graph seed mark://%s%s returned %s", host, legacyPath, result.Response.Status)
		return false
	}
	nodes, edges, parseErr := graphstore.ParseExportStrict(result.Response.Body)
	if parseErr != nil {
		log.Printf("warning: legacy graph seed parse mark://%s%s: %v", host, legacyPath, parseErr)
		return false
	}
	h.graphStore.ReplaceSeed(host, nodes, edges)
	if etag := result.Response.Metadata["etag"]; etag != "" {
		h.graphStore.SetSeedEtag(host, etag)
	}
	if err := h.graphStore.Save(); err != nil {
		log.Printf("warning: graph seed save: %v", err)
	}
	return true
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

// hostInstructions tells the model how to address documents. It rides the
// server instructions once instead of every tool description.
func hostInstructions(host string) string {
	if host != "" {
		return fmt.Sprintf("Connected to %s; use bare paths like /index.md.", host)
	}
	return "Use full mark:// URLs, e.g. mark://host/index.md."
}

func urlDesc(host string) string {
	if host != "" {
		return "path, e.g. /index.md"
	}
	return "mark:// URL, e.g. mark://host/index.md"
}

// outlineThreshold is the body size (bytes) above which mark_fetch returns
// an outline instead of the full body, unless force=true or a #section is
// requested.
const outlineThreshold = mdoutline.OutlineThreshold

func markLookupTool(host string) mcp.Tool {
	options := append([]mcp.ToolOption{
		mcp.WithDescription(mcpfmt.LookupDescription),
		mcp.WithString("url", mcp.Required(), mcp.Description(mcpfmt.ScopeDesc("/")+"; "+urlDesc(host))),
	}, mcpfmt.LookupParams()...)
	return mcp.NewTool("mark_lookup", options...)
}

func markDiscoverTool() mcp.Tool {
	return mcp.NewTool("mark_discover",
		mcp.WithDescription(mcpfmt.DiscoverDescription),
		mcp.WithString("url", mcp.Description("server mark:// URL (optional with -host)")),
	)
}

func markResolveTool(host string) mcp.Tool {
	return mcp.NewTool("mark_resolve",
		mcp.WithDescription(mcpfmt.ResolveDescription),
		mcp.WithString("hash", mcp.Required(), mcp.Description(mcpfmt.HashDesc)),
		mcp.WithString("index", mcp.Required(), mcp.Description("hub hash index document: mark:// URL or "+urlDesc(host))),
	)
}

func markIndexTool(host string) mcp.Tool {
	return mcp.NewTool("mark_index",
		mcp.WithDescription(mcpfmt.IndexDescription),
		mcp.WithString("source", mcp.Required(), mcp.Description("server to crawl: mark:// URL or "+urlDesc(host))),
		mcp.WithString("target", mcp.Required(), mcp.Description("index destination: mark:// URL or "+urlDesc(host))),
		mcp.WithNumber("expected_version", mcp.Description(mcpfmt.IndexVersionDesc)),
		mcp.WithBoolean("dry_run", mcp.Description(mcpfmt.DryRunDesc)),
		mcp.WithBoolean("force", mcp.Description(mcpfmt.IndexForceDesc)),
	)
}

// publisherMeta merges the optional "metadata" argument (values coerced to
// strings) over the agent identity; a caller-supplied "agent" key is skipped
// so identity cannot be spoofed. The server validates keys and values.
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

// agentMeta is the "agent" publisher key: the MCP client name from the
// session context, "unknown" when unavailable.
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
	opts := mcpfmt.Fetch.Options(&req)

	host, path, err := h.resolveURL(docURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	result, err := h.client.Fetch(host, path, h.resolveToken(host))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch failed: %v", err)), nil
	}
	if result.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultText(mcpfmt.Format(result, opts)), nil
	}

	body := result.Response.Body
	version := result.Response.Metadata["version"]
	etag := result.Response.Metadata["etag"]
	key := host + path

	// Binary/non-UTF-8 body: always a notice, never bytes. MCP text can't carry
	// binary faithfully (JSON mangles it); byte-exact retrieval is the CLI's job.
	if mdoutline.BinaryBody(body) {
		return mcp.NewToolResultText(mcpfmt.FormatWith(result, mdoutline.NonMarkdownNotice(len(body)),
			map[string]string{"mode": "binary"}, opts)), nil
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
		return mcp.NewToolResultText(mcpfmt.FormatWith(result, section,
			map[string]string{"section": "#" + anchor}, opts)), nil
	}

	// Dedup needs at least one identity field: with version and etag both
	// absent, two different bodies would compare equal and a changed
	// document would be silently reported as unchanged.
	cur := fetchdedup.Doc{Version: version, Etag: etag}
	prev, seenBefore := h.seenLookup(key)
	if seenBefore && !force && cur.Identified() && prev == cur {
		return mcp.NewToolResultText(fetchdedup.UnchangedNotice(cur, opts.Verbose)), nil
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
		return mcp.NewToolResultText(mcpfmt.FormatWith(result, mdoutline.OutlineBody(docURL, body), extra, opts)), nil
	}

	if cur.Identified() {
		h.seenRecord(key, cur)
	}
	return mcp.NewToolResultText(mcpfmt.FormatWith(result, body, extra, opts)), nil
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

	return mcp.NewToolResultText(mcpfmt.Full(result, "modified")), nil
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

	return mcp.NewToolResultText(mcpfmt.Full(result, "total", "current", "chain-valid", "chain-error")), nil
}

func (h *handler) markLookup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
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
		Match:  req.GetString("match", ""),
	}
	result, err := h.client.Lookup(host, scope, query, h.resolveToken(host), opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("lookup failed: %v", err)), nil
	}
	render := mcpfmt.Lookup.Options(&req)
	text := mcpfmt.Format(result, render) + mcpfmt.CatalogFallback(opts, result)
	if budget := lookupexpand.Budget(&req); budget > 0 && result.Response.Status == protocol.StatusOK {
		token := h.resolveToken(host)
		text += lookupexpand.Expand(ctx, result.Response.Body, query, budget, func(ctx context.Context, path string) (string, error) {
			return fetchBody(h.client.FetchContext(ctx, host, path, token))
		})
	}
	return mcp.NewToolResultText(text), nil
}

// fetchBody adapts a fetch result to the expansion's body-or-error contract.
func fetchBody(r fetch.Result, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if r.Response.Status != protocol.StatusOK {
		return "", errors.New(r.Response.Status)
	}
	return r.Response.Body, nil
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

	onConflict, err := merge.ParseOnConflict(req.GetString("on_conflict", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	meta := publisherMeta(ctx, req.GetArguments())
	// Warn-only narrowing gate, run after a write that landed.
	gate := func(status string) string {
		if !protocol.IsWriteSuccess(status) {
			return ""
		}
		note, gateErr := metaguard.Gate(ctx, expectedVersion, meta, func(ctx context.Context) (fetch.Result, error) {
			return h.client.FetchContext(ctx, host, index.VersionPath(path, expectedVersion), token)
		})
		if gateErr != nil {
			log.Printf("warning: publish metadata check mark://%s%s: %v", host, path, gateErr)
		}
		return note
	}
	if onConflict == merge.OnConflictMerge {
		adapter := &mergeClientAdapter{inner: h.client, host: host, token: token}
		outcome, mErr := merge.Candidate(adapter, path, body, expectedVersion, meta)
		if mErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", mErr)), nil
		}
		return mcp.NewToolResultText(formatOutcome(&outcome) + gate(outcome.Publish.Status)), nil
	}

	result, err := h.client.Publish(host, path, body, token, expectedVersion, meta)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", err)), nil
	}
	return mcp.NewToolResultText(mcpfmt.Full(result, "version", "modified", "server-version") + gate(result.Response.Status)), nil
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

// formatOutcome renders a merge outcome in the tool text shape. OutcomeOK is
// byte-identical to a plain mark_publish; OutcomeCandidate carries the merge
// metadata then the candidate body.
func formatOutcome(o *merge.Outcome) string {
	switch o.Status {
	case merge.OutcomeOK:
		return mcpfmt.Full(fetch.Result{
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

	return mcp.NewToolResultText(mcpfmt.Full(result, "version")), nil
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

	return mcp.NewToolResultText(mcpfmt.Full(result, "version", "modified", "server-version")), nil
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

	return mcp.NewToolResultText(mcpfmt.Full(result, "version", "modified")), nil
}

func (h *handler) markResolve(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	hash, err := req.RequireString("hash")
	if err != nil {
		return mcp.NewToolResultError("hash is required"), nil
	}
	cleanHash, ok := protocol.IsHashPath(hash)
	if !ok {
		return mcp.NewToolResultError("invalid hash format: expected sha256-<64 lowercase hex characters>"), nil
	}
	hash = cleanHash

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

	// V2 manifests fetch only matching prefix shards; legacy indexes stay inline.
	matches, err := index.EntriesForHash(indexPath, indexResult.Response.Body, hash, func(shardPath string) (protocol.Response, error) {
		result, err := h.client.Fetch(indexHost, shardPath, h.resolveToken(indexHost))
		return result.Response, err
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid index: %v", err)), nil
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
		return mcp.NewToolResultText(mcpfmt.Full(result, "version", "modified", "content-hash")), nil
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

	indexedAt := timeNow()
	body := index.Build(sourceScheme, indexedAt, entries)

	if dryRun {
		var b strings.Builder
		for _, w := range warnings {
			b.WriteString(w + "\n")
		}
		fmt.Fprintf(&b, "Indexed %d documents from %s (dry run, logical entry preview only; publication writes a v2 manifest and shards)\n\n", len(entries), sourceScheme)
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

	manifestSource := sourceScheme
	logicalEntries := entries
	// Merge with an existing legacy index or verified sharded generation.
	if expectedVersion > 0 {
		existing, err := h.client.FetchContext(ctx, targetHost, targetPath, h.resolveToken(targetHost))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to fetch existing index: %v", err)), nil
		}
		if existing.Response.Status != protocol.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("failed to fetch existing index: %s", existing.Response.Status)), nil
		}
		existingEntries, err := index.LoadEntries(targetPath, existing.Response.Body, func(shardPath string) (protocol.Response, error) {
			result, err := h.client.FetchContext(ctx, targetHost, shardPath, h.resolveToken(targetHost))
			return result.Response, err
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read existing index: %v", err)), nil
		}
		logicalEntries = index.Merge(existingEntries, sourceScheme, entries)
		manifestSource = "mark://" + targetHost
	}

	publishResult, err := index.PublishGeneration(ctx, index.PublishOptions{
		ManifestPath:            targetPath,
		Source:                  manifestSource,
		Indexed:                 indexedAt,
		Entries:                 logicalEntries,
		ExpectedManifestVersion: &expectedVersion,
	}, func(ioCtx context.Context, docPath string) (protocol.Response, error) {
		result, err := h.client.FetchContext(ioCtx, targetHost, docPath, h.resolveToken(targetHost))
		return result.Response, err
	}, func(ioCtx context.Context, docPath, body string, expected int) (protocol.Response, error) {
		result, err := h.client.PublishContext(ioCtx, targetHost, docPath, body, token, expected, agentMeta(ctx))
		return result.Response, err
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("publish failed: %v", err)), nil
	}

	var b strings.Builder
	for _, w := range warnings {
		b.WriteString(w + "\n")
	}
	fmt.Fprintf(&b, "Indexed %d documents from %s\n", len(entries), sourceScheme)
	fmt.Fprintf(&b, "status: ok\nversion: %d\nshards-published: %d\nshards-reused: %d\n",
		publishResult.ManifestVersion, publishResult.ShardsPublished, publishResult.ShardsReused)
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
	h.seedGraph(ctx, host)

	g, err := h.graphStore.CrawlAndPersist(ctx, startURL, h.graphFetch, fetch.ParseMarkURL, graphstore.CrawlOptions{
		MaxDepth: depth,
		MaxNodes: 200,
		Workers:  5,
	})
	if err != nil && g == nil {
		return mcp.NewToolResultError(fmt.Sprintf("crawl failed: %v", err)), nil
	}
	text := formatGraph(g, startURL)
	if warning := graph.CrawlWarning(err, g.Outcome); warning != nil {
		text += fmt.Sprintf("\nwarning: %v\n", warning)
	}
	return mcp.NewToolResultText(text), nil
}

// formatGraph renders a graph as a plain-text summary for LLM consumption.
func formatGraph(g *graph.Graph, startURL string) string {
	return graph.Summary(g, startURL)
}

func (h *handler) markBacklinks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
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

	h.seedGraph(ctx, host)

	freshness := h.revalidateBacklinks(ctx, fullURL)
	backlinks := h.graphStore.BacklinksEnriched(fullURL)
	if len(backlinks) == 0 {
		return mcp.NewToolResultText(
			freshness + fmt.Sprintf("No backlinks found for %s\nRun mark_graph to populate the graph store.", fullURL),
		), nil
	}

	var b strings.Builder
	b.WriteString(freshness)
	fmt.Fprintf(&b, "Backlinks for %s (%d):\n\n", fullURL, len(backlinks))
	for _, bl := range backlinks {
		ann := graph.EdgeAnnotation(bl.Rel, bl.Label, bl.Anchor, bl.Count) + bl.Observation.Annotation()
		if bl.Title != "" {
			fmt.Fprintf(&b, "- [%s](%s)%s\n", bl.Title, bl.URL, ann)
		} else {
			fmt.Fprintf(&b, "- %s%s\n", bl.URL, ann)
		}
	}
	return mcp.NewToolResultText(b.String()), nil
}

// defaultGraphRetention bounds the published graph's version history: it is a
// generated artifact republished wholesale (one live document reached 545
// versions), and 20 versions is enough to debug a bad crawl.
const defaultGraphRetention = 20

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
	b.WriteString(mcpfmt.Full(result, "version", "modified", "server-version"))
	return mcp.NewToolResultText(b.String()), nil
}
