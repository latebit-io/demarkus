package broker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// This file is the broker's port of the local demarkus-mcp fetch
// ergonomics (client/cmd/demarkus-mcp markFetch): size-adaptive outline
// mode, #section slicing, and unchanged-fetch dedup. The mode-selection
// logic mirrors the local handler the same way formatToolResult mirrors
// formatResult — deliberately, so the two surfaces answer identically —
// while the identity semantics and notice texts are genuinely shared via
// client/fetchdedup (one copy, not a mirror).
// The one structural difference is dedup scope: the local client is a
// per-user process, so a process-lifetime map IS session scope; the
// broker is multi-tenant, so dedup state is keyed by MCP session and a
// caller without a session gets no dedup at all (never a cross-user
// "unchanged" claim).

// outlineThreshold is the body size (bytes) above which mark_fetch
// returns an outline instead of the full body, unless force=true or a
// #section is requested. Matches client/cmd/demarkus-mcp.
const outlineThreshold = 8 * 1024

// sessionSeen is the per-session fetch dedup state: MCP session ID →
// (world+path → identity of the version whose full body that session
// already received). The OnUnregisterSession hook drops a session's
// state, but mcp-go's Streamable HTTP transport only unregisters on an
// explicit session DELETE — a client that just drops the connection
// leaks its entry (mark3labs/mcp-go#723). The caps below therefore WILL
// be reached on a long-lived pod: the session cap evicts the
// least-recently-used session (benign — dedup is best-effort, the cost
// is one extra full read for an evicted-but-live session) and logs so
// the degradation is observable.
type sessionSeen struct {
	mu   sync.Mutex
	byID map[string]*sessionEntry
	log  *slog.Logger
	now  func() time.Time
}

// sessionEntry carries one session's dedup state plus the LRU stamp the
// session-cap eviction orders by.
type sessionEntry struct {
	docs       map[string]fetchdedup.Doc
	lastAccess time.Time
	capLogged  bool
}

const (
	// maxSeenSessions bounds how many sessions carry dedup state. At
	// the cap a new session evicts the least-recently-used one.
	maxSeenSessions = 4096
	// maxSeenDocsPerSession bounds per-session growth; past it the
	// session keeps its existing entries but records no new ones
	// (logged once per session).
	maxSeenDocsPerSession = 1024
)

func newSessionSeen(log *slog.Logger, now func() time.Time) *sessionSeen {
	return &sessionSeen{byID: make(map[string]*sessionEntry), log: log, now: now}
}

func (s *sessionSeen) lookup(sessionID, key string) (fetchdedup.Doc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[sessionID]
	if !ok {
		return fetchdedup.Doc{}, false
	}
	e.lastAccess = s.now()
	d, ok := e.docs[key]
	return d, ok
}

func (s *sessionSeen) record(sessionID, key string, d fetchdedup.Doc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[sessionID]
	if !ok {
		if len(s.byID) >= maxSeenSessions {
			s.evictOldestLocked()
		}
		e = &sessionEntry{docs: make(map[string]fetchdedup.Doc)}
		s.byID[sessionID] = e
	}
	e.lastAccess = s.now()
	if _, exists := e.docs[key]; !exists && len(e.docs) >= maxSeenDocsPerSession {
		if !e.capLogged {
			e.capLogged = true
			s.log.Warn("mcp fetch dedup: per-session doc cap reached; further documents in this session will not dedup",
				"cap", maxSeenDocsPerSession)
		}
		return
	}
	e.docs[key] = d
}

// evictOldestLocked removes the least-recently-used session's state to
// make room for a new one. Caller holds s.mu. Session IDs are
// deliberately not logged (they route Streamable HTTP traffic).
func (s *sessionSeen) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for id, e := range s.byID {
		if oldestID == "" || e.lastAccess.Before(oldest) {
			oldestID, oldest = id, e.lastAccess
		}
	}
	if oldestID == "" {
		return
	}
	delete(s.byID, oldestID)
	s.log.Warn("mcp fetch dedup: session cap reached; evicted least-recently-used session state (likely leaked sessions from clients that disconnected without a session DELETE)",
		"cap", maxSeenSessions)
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
	cur := fetchdedup.Doc{Version: version, Etag: etag}
	sessionID := sessionIDFromContext(ctx)

	var prev fetchdedup.Doc
	seenBefore := false
	if sessionID != "" {
		prev, seenBefore = g.fetchSeen.lookup(sessionID, key)
	}
	if seenBefore && !force && cur.Identified() && prev == cur {
		return mcp.NewToolResultText(fetchdedup.UnchangedNotice(cur)), nil
	}

	extra := map[string]string{}
	if seenBefore && prev != cur {
		extra["note"] = fetchdedup.ChangedNote(prev, cur)
	}

	// Size gate: large documents return an outline unless forced.
	if !force && len(body) >= outlineThreshold {
		extra["mode"] = "outline"
		extra["size"] = fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1)
		return mcp.NewToolResultText(formatToolResultWith(result, mdoutline.OutlineBody(docURL, body),
			extra, "version", "modified", "etag")), nil
	}

	if sessionID != "" && cur.Identified() {
		g.fetchSeen.record(sessionID, key, cur)
	}
	if len(extra) == 0 {
		return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "etag")), nil
	}
	return mcp.NewToolResultText(formatToolResultWith(result, body, extra, "version", "modified", "etag")), nil
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
