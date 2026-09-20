package render

import (
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// ListTruncatedNote closes a LIST page that has a successor.
const ListTruncatedNote = "\n*...truncated, too many entries*\n"

// ListEntry is one immediate child on a LIST page.
type ListEntry struct {
	Name  string
	IsDir bool
}

// ListMetadata is the page metadata; an empty nextCursor means the last page.
func ListMetadata(entries int, nextCursor string) map[string]string {
	meta := map[string]string{
		"entries":  strconv.Itoa(entries),
		"complete": strconv.FormatBool(nextCursor == ""),
	}
	if nextCursor != "" {
		meta["next-cursor"] = nextCursor
	}
	return meta
}

// ListResponse renders one whole LIST page. Entries must already be in
// strictly increasing name order; the server's size cap is not applied.
func ListResponse(dirPath string, entries []ListEntry, nextCursor string) protocol.Response {
	var body strings.Builder
	body.WriteString(IndexHeading(dirPath))
	for _, entry := range entries {
		body.WriteString(EntryLine(entry.Name, entry.IsDir))
	}
	if nextCursor != "" {
		body.WriteString(ListTruncatedNote)
	}
	return protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: ListMetadata(len(entries), nextCursor),
		Body:     body.String(),
	}
}
