package gateway

import (
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// One log line per whole-world lookup, found or not, so the logs count how
// often a search finds nothing before anything is built on it. No query
// text or identity is logged.

// wholeWorldSubject reports a lookup asking a subject of a whole world; a
// filter, the match-all query or a narrower scope asks something else.
func wholeWorldSubject(req *fetch.LookupRequest) bool {
	return (req.Scope == "" || req.Scope == "/") && strings.TrimSpace(req.Query) != "*" &&
		strings.TrimSpace(req.Filter) == "" && req.Match != fetch.MatchSimilar
}

// lookedUp logs a mark_lookup outcome. A body request answered from the
// catalog proves nothing, so it is not logged; nor is a personal world.
func (g *Gateway) lookedUp(req fetch.LookupRequest, r fetch.Result) {
	if g.profile.TenantScoped || !wholeWorldSubject(&req) || r.Response.Status != protocol.StatusOK || fetch.AnsweredFromCatalog(req, r) {
		return
	}
	matches, err := strconv.Atoi(r.Response.Metadata["matches"])
	if err != nil {
		g.deps.Log.Warn("lookup outcome: answer without a matches count", "world", req.Host)
		return
	}
	g.deps.Log.Info("lookup outcome", "tool", "mark_lookup", "world", req.Host, "found", matches > 0)
}

// logLookupAll logs a mark_lookup_all outcome; a partial answer is not.
func (g *Gateway) logLookupAll(report *lookupAllReport, lookup *fetch.LookupRequest) {
	if !wholeWorldSubject(lookup) || report.worlds == 0 || len(report.failures) > 0 || len(report.catalogWorlds) > 0 {
		return
	}
	g.deps.Log.Info("lookup outcome", "tool", "mark_lookup_all", "worlds", report.worlds, "found", len(report.matches) > 0)
}
