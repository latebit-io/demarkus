// Package fetchtest holds client test doubles that serve the server's own
// wire rendering, so a consumer test never hand writes a format.
package fetchtest

import (
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listing"
)

// ListPage is the server's rendering of one LIST page of dir. A name with a
// trailing slash is a directory; nextCursor is empty on the last page.
func ListPage(dir, nextCursor string, names ...string) fetch.Result {
	entries := make([]listing.Entry, 0, len(names))
	for _, name := range names {
		entries = append(entries, listing.Entry{
			Name:  strings.TrimSuffix(name, "/"),
			IsDir: strings.HasSuffix(name, "/"),
		})
	}
	return fetch.Result{Response: listing.RenderPage(dir, entries, nextCursor)}
}
