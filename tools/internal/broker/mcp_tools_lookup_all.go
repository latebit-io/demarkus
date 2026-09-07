package broker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
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
func (g *mcpGateway) handleMarkLookupAll(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	if _, ok := claimsFromCtx(ctx); !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
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

	worlds := readableWorlds(g.srv.cfg)
	opts := fetch.LookupOptions{
		Filter: req.GetString("filter", ""),
		Limit:  limit,
		Match:  req.GetString("match", ""),
	}
	// The client validates per request, which never runs with no readable
	// worlds and would otherwise surface as an all-worlds failure.
	if _, err := protocol.ParseMatch(opts.Match); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	results := g.lookupAllWorlds(ctx, worlds, scope, query, opts)
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
		body: opts.Match == fetch.MatchBody, catalogWorlds: catalogWorlds,
	}
	return mcp.NewToolResultText(mcpfmt.Format(report.result(), mcpfmt.LookupAll.Options(&req))), nil
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

func (g *mcpGateway) lookupAllWorlds(ctx context.Context, worlds []WorldConfig, scope, query string, opts fetch.LookupOptions) []lookupAllWorldResult {
	jobs := make(chan WorldConfig)
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
				result, err := g.dispatcher.LookupContext(ctx, world.Name, scope, query, "", opts)
				if err == nil && result.Response.Status != protocol.StatusOK {
					err = fmt.Errorf("status %s%s", result.Response.Status, lookupFailureDetail(result.Response.Body))
				}
				var matches []lookupAllMatch
				if err == nil {
					matches, err = parseLookupAllMatches(world.Name, result)
				}
				results <- lookupAllWorldResult{world: world.Name, matches: matches, catalog: fetch.AnsweredFromCatalog(opts, result), err: err}
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

func completeCanceledLookupResults(out []lookupAllWorldResult, worlds []WorldConfig, err error) []lookupAllWorldResult {
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
	lines := strings.Split(strings.ReplaceAll(result.Response.Body, "\r\n", "\n"), "\n")
	header, columns := -1, 0
	for i, line := range lines {
		cells, ok := lookuptable.SplitRow(line)
		if ok && lookuptable.IsHeader(cells) {
			header, columns = i, len(cells)
			break
		}
	}
	if header < 0 {
		return nil, fmt.Errorf("malformed LOOKUP response: result table missing")
	}

	matches := make([]lookupAllMatch, 0)
	for _, line := range lines[header+1:] {
		if strings.TrimSpace(line) == "" {
			if len(matches) > 0 {
				break
			}
			continue
		}
		cells, ok := lookuptable.SplitRow(line)
		if !ok {
			break
		}
		if lookuptable.IsSeparator(cells) {
			continue
		}
		if len(cells) != columns {
			return nil, fmt.Errorf("malformed LOOKUP response: row has %d columns, header %d", len(cells), columns)
		}
		importance, err := strconv.ParseFloat(lookuptable.Unescape(cells[1]), 64)
		if err != nil || math.IsNaN(importance) || math.IsInf(importance, 0) || importance < 0 || importance > 1 {
			return nil, fmt.Errorf("malformed LOOKUP response: invalid importance %q", cells[1])
		}
		matchPath, anchor := lookuptable.SplitLocation(cells[0])
		if !strings.HasPrefix(matchPath, "/") {
			return nil, fmt.Errorf("malformed LOOKUP response: invalid path %q", matchPath)
		}
		match := lookupAllMatch{
			world:      world,
			path:       matchPath,
			anchor:     anchor,
			importance: importance,
			title:      lookuptable.Unescape(cells[2]),
			tags:       lookuptable.Unescape(cells[3]),
			rank:       len(matches),
		}
		if columns == lookuptable.BodyColumns {
			match.snippet = lookuptable.Unescape(cells[4])
		}
		matches = append(matches, match)
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
		fmt.Fprintf(&b, "\nnote: answered from the catalog (no body match): %s\n", strings.Join(r.catalogWorlds, ", "))
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

var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkLookupAll
