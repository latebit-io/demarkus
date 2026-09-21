// Command demarkus-mcp is an MCP server that exposes the Mark Protocol as tools
// for LLM agents. It supports fetching documents, listing directories, and
// crawling link graphs via stdio transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/internal/cache"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
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
	if *defaultHost == "" && (*token != "" || os.Getenv("DEMARKUS_AUTH") != "") {
		log.Printf("warning: -token and DEMARKUS_AUTH apply to the -host server only; none is set, so they are unused")
	}
	h := &handler{client: client, defaultHost: *defaultHost, token: *token, graphStore: gs}
	s.AddTools(h.profileTools(*defaultHost, *profile)...)

	registerResources(s, h, *defaultHost)
	registerPrompts(s, *defaultHost)
	if *defaultHost != "" {
		// Best-effort picker population; never blocks or fails startup.
		// Best effort for the life of the process; each LIST has the client's own timeout.
		go registerListedResources(context.Background(), s, h, *defaultHost)
	}

	if err := mcpserver.ServeStdio(s); err != nil {
		log.Fatal(err)
	}
}

func routeDefaultHost(opts *fetch.Options, defaultHost, dialAddress string) error {
	if dialAddress == "" {
		return nil
	}
	host, err := links.DialHost(defaultHost)
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

type handler struct {
	client      marktools.Backend
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
	result, err := h.client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: graphstore.SnapshotManifestPath, Token: token, IfNoneMatch: etag})
	if err != nil {
		log.Printf("warning: graph snapshot fetch mark://%s%s: %v", host, graphstore.SnapshotManifestPath, err)
		return false
	}
	if result.Response.Status == protocol.StatusNotModified {
		return true
	}
	if result.Response.Status == protocol.StatusOK {
		nodes, edges, loadErr := graphstore.LoadSnapshot(graphstore.SnapshotManifestPath, result.Response, func(shardPath string) (protocol.Response, error) {
			shard, fetchErr := h.client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: shardPath, Token: token})
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

	result, err = h.client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: graphstore.LegacyExportPath, Token: token, IfNoneMatch: etag})
	if err != nil {
		log.Printf("warning: graph seed fetch mark://%s%s: %v", host, graphstore.LegacyExportPath, err)
		return false
	}
	if result.Response.Status == protocol.StatusNotFound {
		return true
	}
	if result.Response.Status == protocol.StatusNotModified {
		return true
	}
	if result.Response.Status != protocol.StatusOK {
		log.Printf("warning: legacy graph seed mark://%s%s returned %s", host, graphstore.LegacyExportPath, result.Response.Status)
		return false
	}
	nodes, edges, parseErr := graphstore.ParseExportStrict(result.Response.Body)
	if parseErr != nil {
		log.Printf("warning: legacy graph seed parse mark://%s%s: %v", host, graphstore.LegacyExportPath, parseErr)
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

// resolveToken scopes the -token flag and DEMARKUS_AUTH to the default host;
// other hosts get their stored token only. Reloads the store on each call so
// changes on disk apply without a restart.
func (h *handler) resolveToken(host string) string {
	cred := tokens.Credential{Explicit: h.token}
	if origin, err := links.ParseMark(h.defaultHost); err == nil {
		cred.Origin = origin.DialHost()
	}
	return tokens.Resolve(cred, host, tokens.LoadDefault())
}

// resolveURL parses a mark:// URL or bare path (when -host is set) into host and path.
func (h *handler) resolveURL(rawURL string) (links.Target, error) {
	if strings.HasPrefix(rawURL, "/") {
		if h.defaultHost == "" {
			return links.Target{}, fmt.Errorf("bare path %q requires -host flag", rawURL)
		}
		rawURL = h.defaultHost + rawURL
	}
	return links.ParseMark(rawURL)
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

// agentName is the "agent" publisher value: the MCP client name from the
// session context, "unknown" when unavailable.
func agentName(ctx context.Context) string {
	if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
		if s, ok := session.(mcpserver.SessionWithClientInfo); ok {
			if n := s.GetClientInfo().Name; n != "" {
				return n
			}
		}
	}
	return "unknown"
}

// Tool handlers.
// Handler signatures are dictated by mcp-go's ToolHandlerFunc type.

func (h *handler) markFetch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	args := marktools.FetchArgs{URL: rawURL, Force: req.GetBool("force", false), Render: mcpfmt.Fetch.Options(&req)}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Fetch(ctx, args) })
}

func (h *handler) markList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	args := marktools.ListArgs{
		URL:             rawURL,
		IncludeArchived: req.GetBool("include_archived", false),
		Cursor:          req.GetString("cursor", ""),
		PageSize:        req.GetArguments()["page_size"],
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.List(ctx, args) })
}

func (h *handler) markVersions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Versions(ctx, rawURL) })
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
	args := marktools.LookupArgs{
		URL:    rawURL,
		Query:  query,
		Filter: req.GetString("filter", ""),
		Limit:  req.GetInt("limit", 0),
		Match:  req.GetString("match", ""),
		Budget: lookupexpand.Budget(&req),
		Render: mcpfmt.Lookup.Options(&req),
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Lookup(ctx, args) })
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
	args := marktools.PublishArgs{URL: rawURL, Body: body, OnConflict: req.GetString("on_conflict", "")}
	// A missing or mistyped expected_version stays nil; the tool refuses it after authorization.
	if version, err := req.RequireInt("expected_version"); err == nil {
		args.ExpectedVersion = &version
	}
	if args.Metadata, err = marktools.MetadataArg(req.GetArguments()); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Publish(ctx, args) })
}

func (h *handler) markArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Archive(ctx, rawURL) })
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
	args := marktools.AppendArgs{URL: rawURL, Body: body, ExpectedVersion: req.GetInt("expected_version", 0)}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Append(ctx, args) })
}

func (h *handler) markDiscover(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	// url is optional: without one the tool reads the -host server's manifest.
	rawURL := req.GetString("url", "")
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Discover(ctx, rawURL) })
}

func (h *handler) markResolve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	hash, err := req.RequireString("hash")
	if err != nil {
		return mcp.NewToolResultError("hash is required"), nil
	}
	args := marktools.ResolveArgs{Hash: hash, Index: req.GetString("index", "")}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.ResolveHash(ctx, args) })
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
	args := marktools.IndexArgs{
		Source: sourceURL, Target: targetURL,
		DryRun:          req.GetBool("dry_run", false),
		Force:           req.GetBool("force", false),
		ExpectedVersion: req.GetInt("expected_version", 0),
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Index(ctx, args) })
}

// timeNow is a variable for testing.
var timeNow = time.Now

func (h *handler) markGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	args := marktools.GraphArgs{URL: rawURL, Depth: req.GetInt("depth", 2)}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Graph(ctx, args) })
}

func (h *handler) markBacklinks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Backlinks(ctx, rawURL) })
}

// defaultGraphRetention bounds the published graph's version history: it is a
// generated artifact republished wholesale (one live document reached 545
// versions), and 20 versions is enough to debug a bad crawl.
const defaultGraphRetention = 20

func (h *handler) markGraphExport(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	return h.run(func(t *marktools.Tools) marktools.Result { return t.GraphExport(ctx) })
}

func (h *handler) markGraphPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	// The store is checked before the arguments: without one there is nothing to publish.
	args := marktools.GraphPublishArgs{URL: req.GetString("url", ""), Retention: req.GetInt("retention", defaultGraphRetention)}
	if version, err := req.RequireInt("expected_version"); err == nil {
		args.ExpectedVersion = &version
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.GraphPublish(ctx, args) })
}
