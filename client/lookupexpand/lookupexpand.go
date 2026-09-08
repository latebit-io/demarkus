// Package lookupexpand turns a LOOKUP table into task context: the matched
// sections' text, in rank order, within a result budget. Both MCP surfaces
// call it after rendering the table, so one lookup call can answer a task
// without a fetch per row.
package lookupexpand

import (
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/mdoutline"
)

// Param is the tool argument: approximate result tokens for the expansion.
const Param = "budget"

// ParamDesc describes the budget argument on both surfaces.
const ParamDesc = "approximate result tokens (4 bytes each); when set, the matched sections' text follows the table in rank order under '## path#anchor' lines, whole sections only, until spent (default 0: table only)"

// bytesPerToken is the budget's unit; the surfaces carry no tokenizer.
const bytesPerToken = 4

// minRemaining stops the walk once no useful section can fit.
const minRemaining = 64

// Fetch returns the body of the document at a table row's location (a bare
// path or a mark:// URL, as the table printed it).
type Fetch func(location string) (string, error)

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

// Expand renders the sections of the table's rows in order, each under a
// "## location" line, skipping any that would not fit whole, until the byte
// budget is spent or the rows run out. A bare-path row on a document over
// the outline threshold expands to the sections holding every query term,
// else its outline. Ends with a note saying how many rows were expanded.
// Fetch failures cost a note line, never the call.
func Expand(table, query string, budgetBytes int, fetch Fetch) string {
	rows := locations(table)
	if len(rows) == 0 || budgetBytes <= 0 {
		return ""
	}
	var b strings.Builder
	remaining := budgetBytes
	bodies := make(map[string]string)
	expanded := 0
	for _, loc := range rows {
		if remaining < minRemaining {
			break
		}
		path, anchor := lookuptable.SplitLocation(loc)
		body, ok := bodies[path]
		if !ok {
			var err error
			body, err = fetch(path)
			if err != nil {
				fmt.Fprintf(&b, "note: %s: %v\n", path, err)
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
				block := "## " + path + "#" + h.Anchor + "\n\n" + strings.TrimSpace(body[h.Start:h.End]) + "\n\n"
				if len(block) > remaining {
					continue
				}
				b.WriteString(block)
				remaining -= len(block)
				n++
			}
			if n == 0 {
				block := "## " + path + "\n\n" + strings.TrimSpace(mdoutline.OutlineBody(path, body)) + "\n\n"
				if len(block) <= remaining {
					b.WriteString(block)
					remaining -= len(block)
					n++
				}
			}
			if n > 0 {
				expanded++
			}
			continue
		}
		text, found := sectionText(body, anchor)
		if !found {
			fmt.Fprintf(&b, "note: %s: section #%s not found\n", path, anchor)
			continue
		}
		block := "## " + loc + "\n\n" + strings.TrimSpace(text) + "\n\n"
		if len(block) > remaining {
			continue
		}
		b.WriteString(block)
		remaining -= len(block)
		expanded++
	}
	fmt.Fprintf(&b, "note: expanded %d of %d rows within the budget\n", expanded, len(rows))
	return "\n" + b.String()
}

// sectionText is the row's section, or the whole document for a bare path.
func sectionText(body, anchor string) (string, bool) {
	if anchor != "" {
		return mdoutline.Section(body, anchor)
	}
	return body, true
}

// matchingSections returns the H2-or-deeper sections whose text holds every
// query term (case-insensitive substrings), in document order.
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
	lastEnd := 0 // a matching subsection inside a matching section is already in
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
