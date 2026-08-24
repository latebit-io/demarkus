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
	rank       int
}

type lookupAllFailure struct {
	world string
	err   error
}

type lookupAllWorldResult struct {
	world   string
	matches []lookupAllMatch
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
	results := g.lookupAllWorlds(ctx, worlds, scope, query, fetch.LookupOptions{
		Filter: req.GetString("filter", ""),
		Limit:  limit,
	})
	matches, failures := collectLookupAllResults(results)
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
	return mcp.NewToolResultText(formatLookupAllResult(query, len(worlds), matches, failures)), nil
}

func (g *mcpGateway) lookupAllWorlds(ctx context.Context, worlds []*WorldConfig, scope, query string, opts fetch.LookupOptions) []lookupAllWorldResult {
	jobs := make(chan *WorldConfig)
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
				results <- lookupAllWorldResult{world: world.Name, matches: matches, err: err}
			}
		})
	}
	go func() {
		for _, world := range worlds {
			if ctx.Err() != nil {
				close(jobs)
				wg.Wait()
				close(results)
				return
			}
			select {
			case jobs <- world:
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

func completeCanceledLookupResults(out []lookupAllWorldResult, worlds []*WorldConfig, err error) []lookupAllWorldResult {
	if err == nil {
		err = errors.New("lookup workers stopped before all worlds returned")
	}
	seen := make(map[string]bool, len(out))
	for _, result := range out {
		seen[result.world] = true
	}
	for _, world := range worlds {
		if !seen[world.Name] {
			out = append(out, lookupAllWorldResult{world: world.Name, err: err})
		}
	}
	return out
}

func collectLookupAllResults(results []lookupAllWorldResult) ([]lookupAllMatch, []lookupAllFailure) {
	var matches []lookupAllMatch
	var failures []lookupAllFailure
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, lookupAllFailure{world: result.world, err: result.err})
			continue
		}
		matches = append(matches, result.matches...)
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].world < failures[j].world })
	return matches, failures
}

func parseLookupAllMatches(world string, result fetch.Result) ([]lookupAllMatch, error) {
	lines := strings.Split(strings.ReplaceAll(result.Response.Body, "\r\n", "\n"), "\n")
	header := -1
	for i, line := range lines {
		cells, ok := splitLookupTableRow(line)
		if ok && len(cells) == 4 && strings.EqualFold(cells[0], "Path") && strings.EqualFold(cells[1], "Importance") && strings.EqualFold(cells[2], "Title") && strings.EqualFold(cells[3], "Tags") {
			header = i
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
		cells, ok := splitLookupTableRow(line)
		if !ok {
			break
		}
		if isLookupTableSeparator(cells) {
			continue
		}
		if len(cells) != 4 {
			return nil, fmt.Errorf("malformed LOOKUP response: row has %d columns", len(cells))
		}
		importance, err := strconv.ParseFloat(unescapeLookupCell(cells[1]), 64)
		if err != nil || math.IsNaN(importance) || math.IsInf(importance, 0) || importance < 0 || importance > 1 {
			return nil, fmt.Errorf("malformed LOOKUP response: invalid importance %q", cells[1])
		}
		matchPath := unescapeLookupCell(cells[0])
		if !strings.HasPrefix(matchPath, "/") {
			return nil, fmt.Errorf("malformed LOOKUP response: invalid path %q", matchPath)
		}
		matches = append(matches, lookupAllMatch{
			world:      world,
			path:       matchPath,
			importance: importance,
			title:      unescapeLookupCell(cells[2]),
			tags:       unescapeLookupCell(cells[3]),
			rank:       len(matches),
		})
	}

	want, err := strconv.Atoi(result.Response.Metadata["matches"])
	if err != nil || want != len(matches) {
		return nil, fmt.Errorf("malformed LOOKUP response: matches metadata does not match table")
	}
	return matches, nil
}

func splitLookupTableRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
		return nil, false
	}
	var cells []string
	start := 1
	for i := 1; i < len(line)-1; i++ {
		if line[i] != '|' || lookupCharEscaped(line, i) {
			continue
		}
		cells = append(cells, strings.TrimSpace(line[start:i]))
		start = i + 1
	}
	cells = append(cells, strings.TrimSpace(line[start:len(line)-1]))
	return cells, true
}

func lookupCharEscaped(s string, at int) bool {
	backslashes := 0
	for i := at - 1; i >= 0 && s[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func isLookupTableSeparator(cells []string) bool {
	if len(cells) != 4 {
		return false
	}
	for _, cell := range cells {
		trimmed := strings.Trim(strings.TrimSpace(cell), ":")
		if len(trimmed) < 3 || strings.Trim(trimmed, "-") != "" {
			return false
		}
	}
	return true
}

func unescapeLookupCell(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.ContainsRune(`\\[]()*_`+"`~#|", rune(s[i+1])) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var lookupCellEscaper = strings.NewReplacer(
	`\`, `\\`,
	`[`, `\[`, `]`, `\]`,
	`(`, `\(`, `)`, `\)`,
	`*`, `\*`, `_`, `\_`,
	"`", "\\`", `~`, `\~`,
	`#`, `\#`, `|`, `\|`,
)

func escapeLookupCell(s string) string {
	return lookupCellEscaper.Replace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
}

func formatLookupAllResult(query string, worldCount int, matches []lookupAllMatch, failures []lookupAllFailure) string {
	var b strings.Builder
	status := "ok"
	if len(failures) > 0 {
		status = "partial"
	}
	fmt.Fprintf(&b, "status: %s\nworlds: %d\nsucceeded: %d\nfailed: %d\nmatches: %d\n", status, worldCount, worldCount-len(failures), len(failures), len(matches))
	fmt.Fprintf(&b, "\n# Lookup matches for \"%s\" across readable worlds\n\n", escapeLookupCell(query))
	b.WriteString("| Path | Importance | Title | Tags |\n|------|------------|-------|------|\n")
	for _, match := range matches {
		fmt.Fprintf(&b, "| %s | %.2f | %s | %s |\n",
			escapeLookupCell(qualifiedLookupURL(match.world, match.path)), match.importance,
			escapeLookupCell(match.title), escapeLookupCell(match.tags))
	}
	if len(failures) > 0 {
		b.WriteString("\n## World failures\n\n| World | Error |\n|-------|-------|\n")
		for _, failure := range failures {
			fmt.Fprintf(&b, "| %s | %s |\n", escapeLookupCell(failure.world), escapeLookupCell(failure.err.Error()))
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
