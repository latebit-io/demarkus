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
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
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

// seedGraph refreshes from the host's published graph so cold backlink
// queries work. Failures keep the last good seed and are logged.
func (h *handler) seedGraph(ctx context.Context, host string) {
	if h.graphStore == nil || h.client == nil || host == "" {
		return
	}
	token := h.resolveToken(host)
	h.graphStore.Seed(ctx, graphstore.SeedSource{
		Owner: host,
		Fetch: func(ctx context.Context, path, ifNoneMatch string) (protocol.Response, error) {
			result, err := h.client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: path, Token: token, IfNoneMatch: ifNoneMatch})
			return result.Response, err
		},
		Problem: func(p graphstore.SeedProblem) { log.Printf("warning: %v", p) },
	})
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

// Tool handlers bind a call to a shared body; mcpbind reads the arguments.
// Handler signatures are dictated by mcp-go's ToolHandlerFunc type.

func (h *handler) markFetch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Fetch(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Fetch(ctx, args) })
}

func (h *handler) markList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.List(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.List(ctx, args) })
}

func (h *handler) markVersions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := mcpbind.URL(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Versions(ctx, rawURL) })
}

func (h *handler) markLookup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Lookup(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Lookup(ctx, args) })
}

func (h *handler) markPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Publish(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Publish(ctx, args) })
}

func (h *handler) markArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := mcpbind.URL(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Archive(ctx, rawURL) })
}

func (h *handler) markAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Append(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Append(ctx, args) })
}

func (h *handler) markDiscover(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	// url is optional: without one the tool reads the -host server's manifest.
	rawURL := mcpbind.OptionalURL(&req)
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Discover(ctx, rawURL) })
}

func (h *handler) markResolve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Resolve(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.ResolveHash(ctx, args) })
}

func (h *handler) markIndex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Index(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Index(ctx, args) })
}

// timeNow is a variable for testing.
var timeNow = time.Now

func (h *handler) markGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Graph(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Graph(ctx, args) })
}

func (h *handler) markBacklinks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := mcpbind.URL(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Backlinks(ctx, rawURL) })
}

func (h *handler) markGraphExport(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	return h.run(func(t *marktools.Tools) marktools.Result { return t.GraphExport(ctx) })
}

func (h *handler) markGraphPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args := mcpbind.GraphPublish(&req)
	return h.run(func(t *marktools.Tools) marktools.Result { return t.GraphPublish(ctx, args) })
}
