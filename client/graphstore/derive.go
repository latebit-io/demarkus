package graphstore

import (
	"net"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// DeriveSource is the row the document fetched at host and docPath adds to
// the graph, and the rel- values that made no edge: mark:// edges off
// loopback, one per {To, Rel}, sorted. Strings are cloned off resp.
func DeriveSource(host, docPath string, resp *protocol.Response) (WorldSource, []graph.RejectedRel) {
	url := links.NodeURL(host, docPath)
	extracted := graph.ExtractDocumentEdges(url, resp.Body, resp.Metadata)
	merged := make([]graph.Edge, 0, len(extracted.Edges))
	at := make(map[[2]string]int, len(extracted.Edges))
	for i := range extracted.Edges {
		edge := &extracted.Edges[i]
		target, ok := graphTarget(edge.To)
		if !ok {
			continue
		}
		key := [2]string{target, edge.Rel}
		if j, seen := at[key]; seen {
			merged[j].Absorb(edge)
			continue
		}
		at[key] = len(merged)
		edge.To = target
		merged = append(merged, *edge)
	}
	source := WorldSource{
		Path:  strings.Clone(docPath),
		Etag:  metaValue(resp.Metadata["etag"]),
		Title: metaValue(resp.Metadata["title"]),
		Edges: make([]WorldEdge, len(merged)),
	}
	if version, err := generation.ResponseVersion(docPath, *resp); err == nil {
		source.Version = version
	}
	if source.Title == "" {
		source.Title = metaValue(links.ExtractTitle(resp.Body))
	}
	for i := range merged {
		edge := &merged[i]
		source.Edges[i] = WorldEdge{
			To: strings.Clone(edge.To), Rel: strings.Clone(edge.Rel),
			Label: strings.Clone(edge.Label), Anchor: strings.Clone(edge.Anchor), Count: edge.Count,
		}
	}
	slices.SortFunc(source.Edges, compareWorldEdges)
	return source, extracted.RejectedRels
}

// LinkCount is the source node's body link count: Count summed over its
// edges without Rel.
func (s *WorldSource) LinkCount() int {
	count := 0
	for i := range s.Edges {
		if s.Edges[i].Rel == "" {
			count += s.Edges[i].Count
		}
	}
	return count
}

// metaValue is v cloned when it can stand as a single metadata value, else "".
func metaValue(v string) string {
	if !protocol.IsValidMetaValue(v) {
		return ""
	}
	return strings.Clone(v)
}

// graphTarget keeps mark:// targets in identity form (h and h:6309 are one
// node) and drops loopback, so a dev link never becomes a phantom node.
// Private and cluster hosts stay: real worlds are addressed by those.
func graphTarget(resolved string) (string, bool) {
	if !strings.HasPrefix(resolved, "mark://") {
		return "", false
	}
	target, err := links.ParseMark(resolved)
	if err != nil || isLoopbackHost(target.DialHost()) {
		return "", false
	}
	return target.NodeURL(), true
}

// isLoopbackHost reports whether a mark:// host (host:port or bare) is
// loopback, "localhost", or the unspecified address.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	// An unbracketed IPv6 with a port (::1:6309) defeats SplitHostPort, so a
	// trailing :port is trimmed only when the head is itself an IP.
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	} else if i := strings.LastIndex(h, ":"); i > 0 && net.ParseIP(h[:i]) != nil {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}
