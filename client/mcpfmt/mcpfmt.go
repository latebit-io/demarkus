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

// FetchKeys lead a fetch or explore rendering, every other key following
// sorted. Neither tool has a lean form: metadata is what a reader judges a
// document by and what a republish must carry.
var FetchKeys = []string{"version", "modified", "etag"}

// Envelope names what a tool with a lean form shows: Lean keys render in
// order and nothing else; verbose leads with the same keys and follows with
// every other key sorted.
type Envelope struct {
	Lean        []string
	CapTags     bool   // lean: cap each table row's Tags cell at TagCap
	VerboseDesc string // description of the verbose tool argument
}

// Envelopes of the tools with a lean form.
var (
	Lookup = Envelope{
		Lean:        []string{"matches", "match"},
		CapTags:     true,
		VerboseDesc: "full tag lists per row (default false)",
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
// show in both modes.
func FormatWith(r fetch.Result, body string, extra map[string]string, o Options) string {
	meta := withExtra(r.Response.Metadata, extra)
	if o.Verbose {
		return render(r.Response.Status, meta, o.Envelope.Lean, true, body)
	}
	keys := append(slices.Clone(o.Envelope.Lean), slices.Sorted(maps.Keys(extra))...)
	if o.Envelope.CapTags {
		body = CapTableTags(body)
	}
	return render(r.Response.Status, meta, keys, false, body)
}

// Full renders every metadata key, the named ones first.
func Full(r fetch.Result, keys ...string) string {
	return FullWith(r, r.Response.Body, nil, keys...)
}

// FullWith is Full with a replacement body and surface-added keys.
func FullWith(r fetch.Result, body string, extra map[string]string, keys ...string) string {
	return render(r.Response.Status, withExtra(r.Response.Metadata, extra), keys, true, body)
}

// withExtra overlays extra on meta without mutating it: the map may belong
// to a cached response.
func withExtra(meta, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return meta
	}
	merged := make(map[string]string, len(meta)+len(extra))
	maps.Copy(merged, meta)
	maps.Copy(merged, extra)
	return merged
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

// Note renders the trailer surfaces append to a tool result.
func Note(text string) string {
	return "\nnote: " + text + "\n"
}

// CatalogFallback is the note appended to a lookup result when body match
// was requested and the server answered from the catalog; else "".
func CatalogFallback(req fetch.LookupRequest, r fetch.Result) string {
	if !fetch.AnsweredFromCatalog(req, r) {
		return ""
	}
	return Note(fetch.CatalogFallbackNote)
}
