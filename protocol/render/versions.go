package render

import (
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// VersionEntry is one row of a VERSIONS body.
type VersionEntry struct {
	Version  int
	Modified time.Time
}

// ChainErrorMessage is the chain-error value of a VERSIONS response.
const ChainErrorMessage = "chain integrity check failed"

// VersionsResponse is the VERSIONS response for a document's history, newest
// first. chainValid false also sets chain-error.
func VersionsResponse(docPath string, versions []VersionEntry, chainValid bool) protocol.Response {
	var body strings.Builder
	body.WriteString(VersionsHeading(docPath))
	for _, v := range versions {
		body.WriteString(VersionLine(docPath, v.Version, v.Modified))
	}
	current := 0
	if len(versions) > 0 {
		current = versions[0].Version
	}
	meta := map[string]string{
		"total":       strconv.Itoa(len(versions)),
		"current":     strconv.Itoa(current),
		"chain-valid": strconv.FormatBool(chainValid),
	}
	if !chainValid {
		meta["chain-error"] = ChainErrorMessage
	}
	return protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: body.String()}
}
