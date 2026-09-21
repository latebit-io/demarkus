package render

import (
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// LookupResponse is the LOOKUP response for rows. match is the request's
// match key; it is echoed only when carried, so empty leaves it out.
func LookupResponse(query, scope string, rows []LookupRow, match string) protocol.Response {
	body := match == protocol.MatchBody
	var sb strings.Builder
	sb.WriteString(LookupHeading(query, scope))
	sb.WriteString(LookupHeader(body))
	for i := range rows {
		sb.WriteString(LookupRowLine(&rows[i], body))
	}
	meta := map[string]string{"matches": strconv.Itoa(len(rows))}
	if match != "" {
		meta["match"] = match
	}
	return protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: sb.String()}
}
