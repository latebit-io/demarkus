package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mark_fetch's body is shared (client/marktools). What is the gateway's own is
// the dedup scope: one MCP session, since a pod serves many agents.

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

// Lookup and Record make sessionSeen the shared fetch body's seen store. A call
// outside an MCP session has nothing to dedup against and records nothing.
func (s *sessionSeen) Lookup(ctx context.Context, key string) (fetchdedup.Doc, bool) {
	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return fetchdedup.Doc{}, false
	}
	return s.lookup(sessionID, key)
}

func (s *sessionSeen) Record(ctx context.Context, key string, d fetchdedup.Doc) {
	if sessionID := sessionIDFromContext(ctx); sessionID != "" {
		s.record(sessionID, key, d)
	}
}

// sessionIDFromContext is "" when the context carries no MCP session.
func sessionIDFromContext(ctx context.Context) string {
	if session := mcpserver.ClientSessionFromContext(ctx); session != nil {
		return session.SessionID()
	}
	return ""
}

// handleMarkFetch answers mark_fetch; dedup is scoped to the MCP session.
func (g *Gateway) handleMarkFetch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	args, err := mcpbind.Fetch(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Fetch(ctx, args) })
}
