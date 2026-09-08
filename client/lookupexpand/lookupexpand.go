// Package lookupexpand appends the matched sections' text to a LOOKUP table
// within a byte budget, so one lookup call can answer a task on both MCP
// surfaces without a fetch per row.
package lookupexpand

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/mdoutline"
)

// Param is the tool argument: approximate result tokens for the expansion.
const Param = "budget"

// ParamDesc describes the budget argument on both surfaces.
const ParamDesc = "approximate result tokens (4 bytes each); when set, the matched sections' text follows the table in rank order under '" + Delimiter + " path#anchor' lines, whole sections only, until spent (default 0: table only)"

// Delimiter opens every expanded block and every note; markdown bodies do
// not start lines with it, so consumers can frame blocks unambiguously.
const Delimiter = ">>>"

// bytesPerToken is the budget's unit; the surfaces carry no tokenizer.
const bytesPerToken = 4

// MaxFetches bounds the documents one expansion fetches, whatever the row
// count: the budget bounds output, this bounds wire calls.
const MaxFetches = 10

// Fetch returns the body of the document at a table row's location (a bare
// path or a mark:// URL, as the table printed it).
type Fetch func(ctx context.Context, location string) (string, error)

// Option declares the budget argument on a tool.
func Option() mcp.ToolOption {
	return mcp.WithNumber(Param, mcp.Description(ParamDesc))
}

// Budget reads the argument as bytes; zero when absent or not positive.
func Budget(req *mcp.CallToolRequest) int {
	if n := req.GetInt(Param, 0); n > 0 {
		return n * bytesPerToken
	}
	return 0
}

// expansion accumulates blocks against the remaining budget.
type expansion struct {
	b         strings.Builder
	remaining int
	expanded  int
}

// add appends a block when it fits whole and reports whether it did.
func (e *expansion) add(loc, text string) bool {
	block := Delimiter + " " + loc + "\n\n" + strings.TrimSpace(text) + "\n\n"
	if len(block) > e.remaining {
		return false
	}
	e.b.WriteString(block)
	e.remaining -= len(block)
	return true
}

// note appends a delimited note line.
func (e *expansion) note(format string, args ...any) {
	fmt.Fprintf(&e.b, Delimiter+" note: "+format+"\n", args...)
}

// Expand renders the rows' sections in order, whole sections only, until the
// budget or MaxFetches is spent or ctx ends; a bare path on a large document
// expands to the sections holding every query term, else its outline.
func Expand(ctx context.Context, table, query string, budgetBytes int, fetch Fetch) string {
	rows := locations(table)
	if len(rows) == 0 || budgetBytes <= 0 {
		return ""
	}
	e := &expansion{remaining: budgetBytes}
	bodies := make(map[string]string)
	fetches := 0
	for _, loc := range rows {
		if e.remaining <= 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			e.note("stopped: %v", err)
			break
		}
		path, anchor := lookuptable.SplitLocation(loc)
		body, seen := bodies[path]
		if !seen {
			if fetches >= MaxFetches {
				e.note("fetch limit of %d documents reached", MaxFetches)
				break
			}
			fetches++
			var err error
			body, err = fetch(ctx, path)
			if err != nil {
				e.note("%s: %v", path, err)
				bodies[path] = ""
				continue
			}
			bodies[path] = body
		}
		if body == "" {
			continue
		}
		if anchor == "" && len(body) >= mdoutline.OutlineThreshold {
			n := 0
			for _, h := range matchingSections(body, query) {
				if e.add(path+"#"+h.Anchor, body[h.Start:h.End]) {
					n++
				}
			}
			if n == 0 && e.add(path, mdoutline.OutlineBody(path, body)) {
				n++
			}
			if n > 0 {
				e.expanded++
			}
			continue
		}
		text := body
		if anchor != "" {
			var found bool
			if text, found = mdoutline.Section(body, anchor); !found {
				e.note("%s: section #%s not found", path, anchor)
				continue
			}
		}
		if e.add(loc, text) {
			e.expanded++
		}
	}
	e.note("expanded %d of %d rows within the budget", e.expanded, len(rows))
	return "\n" + e.b.String()
}

// matchingSections returns the H2-or-deeper sections whose text holds every
// query term (case-insensitive substrings), outermost only, in order.
func matchingSections(body, query string) []mdoutline.Heading {
	var terms []string
	for t := range strings.FieldsSeq(strings.ToLower(query)) {
		if t = strings.Trim(t, "\"'.,;:()"); len(t) >= 2 {
			terms = append(terms, t)
		}
	}
	if len(terms) == 0 {
		return nil
	}
	var out []mdoutline.Heading
	lastEnd := 0
	for _, h := range mdoutline.Headings(body) {
		if h.Level < 2 || h.Start < lastEnd {
			continue
		}
		text := strings.ToLower(body[h.Start:h.End])
		hit := true
		for _, t := range terms {
			if !strings.Contains(text, t) {
				hit = false
				break
			}
		}
		if hit {
			out = append(out, h)
			lastEnd = h.End
		}
	}
	return out
}

// locations returns the first cell of every data row, in table order,
// keeping the anchor.
func locations(table string) []string {
	var out []string
	for line := range strings.SplitSeq(table, "\n") {
		cells, ok := lookuptable.SplitRow(line)
		if !ok || !lookuptable.IsDataRow(cells) {
			continue
		}
		out = append(out, lookuptable.Unescape(cells[0]))
	}
	return out
}
