// Package mcpfmt renders a wire response as MCP tool text. demarkus-mcp and
// the broker share it, which keeps the two surfaces byte-identical.
package mcpfmt

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/mark3labs/mcp-go/mcp"
)

// TagCap is how many tags a lean lookup row shows before "+N more".
const TagCap = 10

// verboseParam is the tool argument that selects the verbose envelope.
const verboseParam = "verbose"

// Envelope names what a tool shows. Lean keys render in order and nothing
// else from the response; Verbose keys lead the full rendering, every other
// key following sorted (nil means the Lean keys lead).
type Envelope struct {
	Lean        []string
	Verbose     []string
	CapTags     bool   // lean: cap each table row's Tags cell at TagCap
	VerboseDesc string // description of the verbose tool argument
}

// Envelopes of the tools with a lean form.
var (
	Fetch = Envelope{
		Lean:        []string{"version", "title"},
		Verbose:     []string{"version", "modified", "etag"},
		VerboseDesc: "every metadata key: etag, content-hash, modified, agent, importance, tags, type, rel-* (default false)",
	}
	Lookup = Envelope{
		Lean:        []string{"matches", "match"},
		CapTags:     true,
		VerboseDesc: "full tag lists per row (default false: ten tags, then +N more)",
	}
	LookupAll = Envelope{
		Lean:        []string{"worlds", "succeeded", "failed", "matches", "match"},
		CapTags:     true,
		VerboseDesc: Lookup.VerboseDesc,
	}
)

// Param declares the verbose argument on a tool.
func (e *Envelope) Param() mcp.ToolOption {
	return mcp.WithBoolean(verboseParam, mcp.Description(e.VerboseDesc))
}

// Options reads the verbose flag from a tool call.
func (e *Envelope) Options(req *mcp.CallToolRequest) Options {
	return Options{Envelope: e, Verbose: req.GetBool(verboseParam, false)}
}

// Options selects the envelope and mode for one rendering.
type Options struct {
	Envelope *Envelope
	Verbose  bool
}

// Format renders r: status line, metadata per the envelope, blank line, body.
func Format(r fetch.Result, o Options) string {
	return FormatWith(r, r.Response.Body, nil, o)
}

// FormatWith renders r with a replacement body and surface-added keys, which
// show in both modes. r's metadata map is never mutated: it may belong to a
// cached response.
func FormatWith(r fetch.Result, body string, extra map[string]string, o Options) string {
	meta := r.Response.Metadata
	if len(extra) > 0 {
		meta = make(map[string]string, len(meta)+len(extra))
		maps.Copy(meta, r.Response.Metadata)
		maps.Copy(meta, extra)
	}
	keys, rest := o.Envelope.Verbose, true
	if keys == nil {
		keys = o.Envelope.Lean
	}
	if !o.Verbose {
		keys = append(slices.Clone(o.Envelope.Lean), slices.Sorted(maps.Keys(extra))...)
		rest = false
		if o.Envelope.CapTags {
			body = CapTableTags(body)
		}
	}
	return render(r.Response.Status, meta, keys, rest, body)
}

// Full renders every metadata key, the named ones first: the rendering of
// tools that have no lean form.
func Full(r fetch.Result, keys ...string) string {
	return render(r.Response.Status, r.Response.Metadata, keys, true, r.Response.Body)
}

// render writes the named keys in order, then (with rest) every other key
// sorted so output is deterministic across map iterations.
func render(status string, meta map[string]string, keys []string, rest bool, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", status)
	for _, key := range keys {
		if v, ok := meta[key]; ok {
			fmt.Fprintf(&b, "%s: %s\n", key, v)
		}
	}
	if rest {
		remaining := make([]string, 0, len(meta))
		for k := range meta {
			if !slices.Contains(keys, k) {
				remaining = append(remaining, k)
			}
		}
		sort.Strings(remaining)
		for _, k := range remaining {
			fmt.Fprintf(&b, "%s: %s\n", k, meta[k])
		}
	}
	if body != "" {
		b.WriteString("\n")
		b.WriteString(body)
	}
	return b.String()
}

// CapTags shortens a comma-separated tag list to TagCap entries plus
// "+N more". A list within the cap is returned unchanged.
func CapTags(tags string) string {
	if strings.Count(tags, ",") < TagCap {
		return tags
	}
	parts := strings.Split(tags, ",")
	kept := make([]string, 0, TagCap+1)
	for _, part := range parts[:TagCap] {
		kept = append(kept, strings.TrimSpace(part))
	}
	kept = append(kept, fmt.Sprintf("+%d more", len(parts)-TagCap))
	return strings.Join(kept, ", ")
}

// CapTableTags applies CapTags to the Tags cell of every result row in a
// lookup table. Rows within the cap, and every other line, pass through
// byte for byte.
func CapTableTags(table string) string {
	lines := strings.Split(table, "\n")
	changed := false
	for i, line := range lines {
		if strings.Count(line, ",") < TagCap {
			continue
		}
		cells, ok := lookuptable.SplitRow(line)
		if !ok || !lookuptable.IsDataRow(cells) {
			continue
		}
		capped := CapTags(cells[3])
		if capped == cells[3] {
			continue
		}
		cells[3] = capped
		lines[i] = lookuptable.JoinRow(cells)
		changed = true
	}
	if !changed {
		return table
	}
	return strings.Join(lines, "\n")
}
