// Package fetchdedup carries the session fetch-dedup identity shared by
// the MCP fetch surfaces (client/cmd/demarkus-mcp and the broker MCP
// gateway): what identifies a fetched document version, and the exact
// notice texts for "unchanged" and "changed since you last read this".
// Both surfaces render these byte-identically; keeping one copy here is
// what guarantees it.
package fetchdedup

import (
	"fmt"
	"strings"
)

// Doc identifies a fetched document version by the identity metadata a
// Mark server returns. Either field may be empty; a Doc with both empty
// carries no identity and must not drive dedup decisions.
type Doc struct {
	Version string
	Etag    string
}

// Identified reports whether d carries at least one identity field.
// With both absent, two different bodies would compare equal and a
// changed document would be silently reported as unchanged.
func (d Doc) Identified() bool {
	return d.Version != "" || d.Etag != ""
}

// UnchangedNotice renders the dedup short-circuit response: the document
// identity plus a pointer at force=true. Only call it for an Identified
// Doc.
func UnchangedNotice(d Doc) string {
	var b strings.Builder
	b.WriteString("status: unchanged\n")
	if d.Version != "" {
		fmt.Fprintf(&b, "version: %s\n", d.Version)
	}
	if d.Etag != "" {
		fmt.Fprintf(&b, "etag: %s\n", d.Etag)
	}
	since := "since this session's earlier fetch (etag match)"
	if d.Version != "" {
		since = "since v" + d.Version
	}
	fmt.Fprintf(&b, "\nunchanged %s; the full body was returned earlier this session; pass force=true to re-read it\n", since)
	return b.String()
}

// ChangedNote describes how the document identity moved since the
// session's earlier full-body fetch. Either side of a version change can
// be empty (recording requires only one identity field): identity-type
// flips are spelled out instead of rendering a bare "v" with no number.
func ChangedNote(prev, cur Doc) string {
	switch {
	case prev.Version != cur.Version && prev.Version == "":
		return fmt.Sprintf("changed since this session's earlier fetch (was etag-only, now v%s)", cur.Version)
	case prev.Version != cur.Version && cur.Version == "":
		return fmt.Sprintf("changed since this session's earlier fetch (was v%s, now etag-only)", prev.Version)
	case prev.Version != cur.Version:
		return fmt.Sprintf("changed since this session's earlier fetch (v%s -> v%s)", prev.Version, cur.Version)
	case cur.Version == "":
		// Etag-only identity throughout (server sends no version).
		return "content changed since this session's earlier fetch (etag differs)"
	}
	return fmt.Sprintf("content changed since this session's earlier fetch (still v%s, etag differs)", cur.Version)
}
