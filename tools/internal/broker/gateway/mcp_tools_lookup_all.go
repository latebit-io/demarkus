package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	defaultLookupAllLimit = 10
	maxLookupAllResults   = 1000
	lookupAllWorkers      = 8
)

type lookupAllMatch struct {
	world      string
	path       string
	importance float64
	title      string
	tags       string
	anchor     string // body mode: section anchor without '#'
	snippet    string // body mode only
	rank       int
}

type lookupAllFailure struct {
	world string
	err   error
}

type lookupAllWorldResult struct {
	world   string
	matches []lookupAllMatch
	catalog bool // body match requested, world answered from its catalog
	err     error
}

// handleMarkLookupAll fans LOOKUP out across readable worlds. A rank-first
// merge preserves each server's relevance ordering without inventing a
// cross-world score the protocol does not expose.
func (g *Gateway) handleMarkLookupAll(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	if _, ok := core.ClaimsFromCtx(ctx); !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	query, err := mcpbind.Required(&req, "query")
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	scope := req.GetString("scope", "/")
	if err := protocol.ValidateRequestPath(scope); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid scope: %v", err)), nil
	}
	limit := req.GetInt("limit", defaultLookupAllLimit)
	if limit < 1 {
		return mcp.NewToolResultError("limit must be at least 1"), nil
	}
	if limit > maxLookupAllResults {
		limit = maxLookupAllResults
	}

	// The tenant gate already refuses this tool in tenant mode; scoping here
	// too means a gate mistake cannot turn it into a cross-tenant search.
	worlds := g.scopedWorlds(ctx)
	lookup := fetch.LookupRequest{
		Scope: scope, Query: query,
		Filter: req.GetString("filter", ""),
		Limit:  limit,
		Match:  req.GetString("match", ""),
	}
	// The client validates per request, which never runs with no readable
	// worlds and would otherwise surface as an all-worlds failure.
	if _, err := protocol.ParseMatch(lookup.Match); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	results := g.lookupAllWorlds(ctx, worlds, lookup)
	matches, failures, catalogWorlds := collectLookupAllResults(results)
	if len(failures) == len(worlds) && len(worlds) > 0 {
		return mcp.NewToolResultError("lookup failed in all worlds: " + formatLookupAllFailureList(failures)), nil
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		if matches[i].importance != matches[j].importance {
			return matches[i].importance > matches[j].importance
		}
		if matches[i].world != matches[j].world {
			return matches[i].world < matches[j].world
		}
		return matches[i].path < matches[j].path
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	report := lookupAllReport{
		query: query, worlds: len(worlds), matches: matches, failures: failures,
		body: lookup.Match == fetch.MatchBody, catalogWorlds: catalogWorlds,
	}
	merged := report.result()
	text := mcpfmt.Format(merged, mcpfmt.LookupAll.Options(&req))
	if budget := lookupexpand.Budget(&req); budget > 0 {
		text += lookupexpand.Expand(ctx, merged.Response.Body, query, budget, func(ctx context.Context, loc string) (string, error) {
			worldName, path, err := parseToolURL(loc)
			if err != nil {
				return "", err
			}
			return marktools.FetchBody(g.dispatcher.Fetch(ctx, fetch.FetchRequest{Host: worldName, Path: path}))
		})
	}
	return mcp.NewToolResultText(text), nil
}

// lookupAllReport is everything the merged table renders.
type lookupAllReport struct {
	query         string
	worlds        int
	matches       []lookupAllMatch
	failures      []lookupAllFailure
	body          bool     // body match requested: render the Snippet column
	catalogWorlds []string // worlds that answered a body request from the catalog
}

// lookupAllWorlds sends lookup to every world; Host is filled in per world.
func (g *Gateway) lookupAllWorlds(ctx context.Context, worlds []core.WorldConfig, lookup fetch.LookupRequest) []lookupAllWorldResult {
	jobs := make(chan core.WorldConfig)
	results := make(chan lookupAllWorldResult, len(worlds))
	workers := min(lookupAllWorkers, len(worlds))

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for world := range jobs {
				select {
				case <-ctx.Done():
					results <- lookupAllWorldResult{world: world.Name, err: ctx.Err()}
					continue
				default:
				}
				perWorld := lookup
				perWorld.Host = world.Name
				result, err := g.dispatcher.Lookup(ctx, perWorld)
				if err == nil && result.Response.Status != protocol.StatusOK {
					err = fmt.Errorf("status %s%s", result.Response.Status, lookupFailureDetail(result.Response.Body))
				}
				var matches []lookupAllMatch
				if err == nil {
					matches, err = parseLookupAllMatches(world.Name, result)
				}
				results <- lookupAllWorldResult{world: world.Name, matches: matches, catalog: fetch.AnsweredFromCatalog(lookup, result), err: err}
			}
		})
	}
	go func() {
		for j := range worlds {
			if ctx.Err() != nil {
				close(jobs)
				wg.Wait()
				close(results)
				return
			}
			select {
			case jobs <- worlds[j]:
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				close(results)
				return
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	out := make([]lookupAllWorldResult, 0, len(worlds))
	for len(out) < len(worlds) {
		select {
		case result, ok := <-results:
			if !ok {
				return completeCanceledLookupResults(out, worlds, ctx.Err())
			}
			out = append(out, result)
		case <-ctx.Done():
			return completeCanceledLookupResults(out, worlds, ctx.Err())
		}
	}
	return out
}

func completeCanceledLookupResults(out []lookupAllWorldResult, worlds []core.WorldConfig, err error) []lookupAllWorldResult {
	if err == nil {
		err = errors.New("lookup workers stopped before all worlds returned")
	}
	seen := make(map[string]bool, len(out))
	for _, result := range out {
		seen[result.world] = true
	}
	for j := range worlds {
		if !seen[worlds[j].Name] {
			out = append(out, lookupAllWorldResult{world: worlds[j].Name, err: err})
		}
	}
	return out
}

func collectLookupAllResults(results []lookupAllWorldResult) (matches []lookupAllMatch, failures []lookupAllFailure, catalogWorlds []string) {
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, lookupAllFailure{world: result.world, err: result.err})
			continue
		}
		if result.catalog {
			catalogWorlds = append(catalogWorlds, result.world)
		}
		matches = append(matches, result.matches...)
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].world < failures[j].world })
	sort.Strings(catalogWorlds)
	return matches, failures, catalogWorlds
}

func parseLookupAllMatches(world string, result fetch.Result) ([]lookupAllMatch, error) {
	table, err := lookuptable.ParseTable(result.Response.Body)
	if err != nil {
		return nil, err
	}
	matches := make([]lookupAllMatch, 0, len(table.Rows))
	for i, row := range table.Rows {
		matches = append(matches, lookupAllMatch{
			world:      world,
			path:       row.Path,
			anchor:     row.Anchor,
			importance: row.Importance,
			title:      row.Title,
			tags:       row.Tags,
			snippet:    row.Snippet,
			rank:       i,
		})
	}

	want, err := strconv.Atoi(result.Response.Metadata["matches"])
	if err != nil || want != len(matches) {
		return nil, fmt.Errorf("malformed LOOKUP response: matches metadata does not match table")
	}
	return matches, nil
}

// result shapes the merged report as a wire-style response so the shared
// envelope renders it like a single-world lookup.
func (r *lookupAllReport) result() fetch.Result {
	status := "ok"
	if len(r.failures) > 0 {
		status = "partial"
	}
	meta := map[string]string{
		"worlds":    strconv.Itoa(r.worlds),
		"succeeded": strconv.Itoa(r.worlds - len(r.failures)),
		"failed":    strconv.Itoa(len(r.failures)),
		"matches":   strconv.Itoa(len(r.matches)),
	}
	if r.body {
		meta["match"] = fetch.MatchBody
	}
	return fetch.Result{Response: protocol.Response{Status: status, Metadata: meta, Body: r.table()}}
}

func (r *lookupAllReport) table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Lookup matches for \"%s\" across readable worlds\n\n", lookuptable.Escape(r.query))
	b.WriteString(lookuptable.Header(r.body))
	for _, match := range r.matches {
		// The anchor rides after escaping: a slug cannot break the table,
		// and the agent hands the row to mark_fetch as is.
		location := lookuptable.Escape(qualifiedLookupURL(match.world, match.path))
		if match.anchor != "" {
			location += "#" + match.anchor
		}
		fmt.Fprintf(&b, "| %s | %.2f | %s | %s |", location, match.importance,
			lookuptable.Escape(match.title), lookuptable.Escape(match.tags))
		if r.body {
			fmt.Fprintf(&b, " %s |", lookuptable.Escape(match.snippet))
		}
		b.WriteString("\n")
	}
	if len(r.catalogWorlds) > 0 {
		b.WriteString(mcpfmt.Note("answered from the catalog (no body match): " + strings.Join(r.catalogWorlds, ", ")))
	}
	if len(r.failures) > 0 {
		b.WriteString("\n## World failures\n\n| World | Error |\n|-------|-------|\n")
		for _, failure := range r.failures {
			fmt.Fprintf(&b, "| %s | %s |\n", lookuptable.Escape(failure.world), lookuptable.Escape(failure.err.Error()))
		}
	}
	return b.String()
}

func qualifiedLookupURL(world, path string) string {
	return (&url.URL{Scheme: "mark", Host: world, Path: path}).String()
}

func formatLookupAllFailureList(failures []lookupAllFailure) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, failure.world+": "+failure.err.Error())
	}
	return strings.Join(parts, "; ")
}

func lookupFailureDetail(body string) string {
	detail := strings.Join(strings.Fields(body), " ")
	if detail == "" {
		return ""
	}
	const maxRunes = 256
	runes := []rune(detail)
	if len(runes) > maxRunes {
		detail = string(runes[:maxRunes]) + "..."
	}
	return ": " + detail
}
