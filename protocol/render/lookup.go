package render

import (
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// Lookup is what a LOOKUP response echoes and which columns it carries.
// Match is the request's match key, echoed only when carried; Pins adds the
// Pins column.
type Lookup struct {
	Query, Scope, Match string
	Pins                bool
}

// LookupResponse is the LOOKUP response for rows, without the Pins column.
func LookupResponse(query, scope string, rows []LookupRow, match string) protocol.Response {
	return Lookup{Query: query, Scope: scope, Match: match}.Response(rows)
}

// Response renders rows under l's heading and columns.
func (l Lookup) Response(rows []LookupRow) protocol.Response {
	columns := Columns{Snippet: protocol.MatchSectioned(l.Match), Pins: l.Pins}
	var sb strings.Builder
	sb.WriteString(LookupHeading(l.Query, l.Scope))
	sb.WriteString(LookupHeader(columns))
	for i := range rows {
		sb.WriteString(LookupRowLine(&rows[i], columns))
	}
	meta := map[string]string{"matches": strconv.Itoa(len(rows))}
	if l.Match != "" {
		meta["match"] = l.Match
	}
	return protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: sb.String()}
}
