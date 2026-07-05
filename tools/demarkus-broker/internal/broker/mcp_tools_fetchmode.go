package broker

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// This file is the broker's port of the local demarkus-mcp fetch
// ergonomics (client/cmd/demarkus-mcp markFetch): size-adaptive outline
// mode, #section slicing, and unchanged-fetch dedup. The mode-selection
// logic mirrors the local handler the same way formatToolResult mirrors
// formatResult — deliberately, so the two surfaces answer identically.
// The one structural difference is dedup scope: the local client is a
// per-user process, so a process-lifetime map IS session scope; the
// broker is multi-tenant, so dedup state is keyed by MCP session and a
// caller without a session gets no dedup at all (never a cross-user
// "unchanged" claim).

// outlineThreshold is the body size (bytes) above which mark_fetch
// returns an outline instead of the full body, unless force=true or a
// #section is requested. Matches client/cmd/demarkus-mcp.
const outlineThreshold = 8 * 1024

// seenDoc identifies a document version already returned in full to one
// MCP session.
type seenDoc struct {
	version string
	etag    string
}

// sessionSeen is the per-session fetch dedup state: MCP session ID →
// (world+path → identity of the version whose full body that session
// already received). Sessions are evicted by the OnUnregisterSession
// hook; the caps below bound memory if a transport never unregisters.
type sessionSeen struct {
	mu   sync.Mutex
	byID map[string]map[string]seenDoc
}

const (
	// maxSeenSessions bounds how many concurrent sessions carry dedup
	// state. Beyond it, new sessions simply get no dedup (correct,
	// just less token-efficient) rather than evicting someone else's.
	maxSeenSessions = 4096
	// maxSeenDocsPerSession bounds per-session growth; past it the
	// session keeps its existing entries but records no new ones.
	maxSeenDocsPerSession = 1024
)

func newSessionSeen() *sessionSeen {
	return &sessionSeen{byID: make(map[string]map[string]seenDoc)}
}

func (s *sessionSeen) lookup(sessionID, key string) (seenDoc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byID[sessionID][key]
	return d, ok
}

func (s *sessionSeen) record(sessionID, key string, d seenDoc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	docs, ok := s.byID[sessionID]
	if !ok {
		if len(s.byID) >= maxSeenSessions {
			return
		}
		docs = make(map[string]seenDoc)
		s.byID[sessionID] = docs
	}
	if _, exists := docs[key]; !exists && len(docs) >= maxSeenDocsPerSession {
		return
	}
	docs[key] = d
}

func (s *sessionSeen) drop(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, sessionID)
}

// sessionIDFromContext returns the MCP session ID for the request, or ""
// when the context carries no session (dedup is then skipped entirely).
func sessionIDFromContext(ctx context.Context) string {
	if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
		return session.SessionID()
	}
	return ""
}

// handleMarkFetch implements the mark_fetch tool against the brokered
// world with the shared fetch ergonomics: full body under 8KB, outline
// above, url#anchor section slicing at any size, force override, and
// session-scoped unchanged dedup. Reads are open to any SSO-authenticated
// caller; see the pre-ergonomics rationale on handleMarkList.
func (g *mcpGateway) handleMarkFetch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	// The fragment is the section selector, cut before URL validation —
	// parseToolURL rejects fragments because the other verbs would
	// silently drop them; here it has defined meaning.
	docURL, anchor, _ := strings.Cut(raw, "#")
	force := req.GetBool("force", false)

	worldName, path, err := parseToolURL(docURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	result, err := g.dispatcher.Fetch(worldName, path, "")
	if err != nil {
		return g.toolErrorFor("fetch", worldName, err), nil
	}
	if result.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "etag")), nil
	}

	body := result.Response.Body
	version := result.Response.Metadata["version"]
	etag := result.Response.Metadata["etag"]
	key := worldName + path

	// #section slice: works at any size and bypasses dedup — the agent
	// is asking for content it has not necessarily seen.
	if anchor != "" {
		section, ok := mdoutline.Section(body, anchor)
		if !ok {
			available := strings.Join(mdoutline.Anchors(body), ", ")
			if available == "" {
				available = "(document has no headings)"
			}
			return mcp.NewToolResultError(fmt.Sprintf("section #%s not found in %s; available anchors: %s", anchor, docURL, available)), nil
		}
		return mcp.NewToolResultText(formatToolResultWith(result, section,
			map[string]string{"section": "#" + anchor}, "version", "modified", "etag")), nil
	}

	// Dedup needs at least one identity field: with version and etag
	// both absent, two different bodies would compare equal and a
	// changed document would be silently reported as unchanged.
	hasIdentity := version != "" || etag != ""
	sessionID := sessionIDFromContext(ctx)

	var prev seenDoc
	seenBefore := false
	if sessionID != "" {
		prev, seenBefore = g.fetchSeen.lookup(sessionID, key)
	}
	if seenBefore && !force && hasIdentity && prev.version == version && prev.etag == etag {
		return mcp.NewToolResultText(unchangedNotice(version, etag)), nil
	}

	extra := map[string]string{}
	if seenBefore && (prev.version != version || prev.etag != etag) {
		extra["note"] = changedNote(prev, version)
	}

	// Size gate: large documents return an outline unless forced.
	if !force && len(body) >= outlineThreshold {
		extra["mode"] = "outline"
		extra["size"] = fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1)
		return mcp.NewToolResultText(formatToolResultWith(result, mdoutline.OutlineBody(docURL, body),
			extra, "version", "modified", "etag")), nil
	}

	if sessionID != "" && hasIdentity {
		g.fetchSeen.record(sessionID, key, seenDoc{version: version, etag: etag})
	}
	if len(extra) == 0 {
		return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "etag")), nil
	}
	return mcp.NewToolResultText(formatToolResultWith(result, body, extra, "version", "modified", "etag")), nil
}

// unchangedNotice renders the dedup short-circuit response: the document
// identity plus a pointer at force=true. Only called when at least one of
// version/etag is non-empty.
func unchangedNotice(version, etag string) string {
	var b strings.Builder
	b.WriteString("status: unchanged\n")
	if version != "" {
		fmt.Fprintf(&b, "version: %s\n", version)
	}
	if etag != "" {
		fmt.Fprintf(&b, "etag: %s\n", etag)
	}
	since := "since this session's earlier fetch (etag match)"
	if version != "" {
		since = "since v" + version
	}
	fmt.Fprintf(&b, "\nunchanged %s — the full body was returned earlier this session; pass force=true to re-read it\n", since)
	return b.String()
}

// changedNote describes how the document identity moved since the
// session's earlier full-body fetch.
func changedNote(prev seenDoc, version string) string {
	if prev.version != version {
		return fmt.Sprintf("changed since this session's earlier fetch (v%s -> v%s)", prev.version, version)
	}
	return fmt.Sprintf("content changed since this session's earlier fetch (still v%s, etag differs)", version)
}

// formatToolResultWith renders r via formatToolResult with a replacement
// body and extra metadata entries. The metadata map is cloned first — r
// may hold a response whose map must never be mutated. Mirrors the local
// client's formatResultWith.
func formatToolResultWith(r fetch.Result, body string, extra map[string]string, keys ...string) string {
	meta := maps.Clone(r.Response.Metadata)
	if meta == nil {
		meta = make(map[string]string, len(extra))
	}
	maps.Copy(meta, extra)
	r.Response.Metadata = meta
	r.Response.Body = body
	return formatToolResult(r, keys...)
}
