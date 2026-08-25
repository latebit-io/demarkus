// Package listing validates and decodes one paginated LIST response.
package listing

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// Entry is one decoded immediate child of the listed directory.
type Entry struct {
	Name  string
	Path  string
	IsDir bool
}

// Page is one validated LIST page. Invalid contains malformed link targets that
// callers may report, but never treat as complete inventory.
type Page struct {
	Entries    []Entry
	Invalid    []string
	Complete   bool
	NextCursor string
	LastName   string
}

// ParsePage validates metadata, count, decoded-name ordering, and containment.
// after is the last valid decoded child name from the preceding page.
func ParsePage(dir string, resp protocol.Response, after string) (Page, error) {
	meta, err := fetch.ParseListPageMetadata(resp)
	if err != nil {
		return Page{}, err
	}
	dests := links.Extract(resp.Body)
	if len(dests) != meta.Entries {
		return Page{}, fmt.Errorf("metadata reports %d entries, body has %d", meta.Entries, len(dests))
	}
	page := Page{Complete: meta.Complete, NextCursor: meta.NextCursor, LastName: after}
	for _, dest := range dests {
		entry, ok := ResolveEntry(dir, dest)
		if !ok {
			page.Invalid = append(page.Invalid, dest)
			continue
		}
		if entry.Name <= page.LastName {
			return Page{}, fmt.Errorf("entries are not strictly ordered")
		}
		page.LastName = entry.Name
		page.Entries = append(page.Entries, entry)
	}
	return page, nil
}

// ResolveEntry decodes and contains one relative LIST link target.
func ResolveEntry(dir, dest string) (Entry, bool) {
	name, err := url.PathUnescape(dest)
	if err != nil {
		return Entry{}, false
	}
	isDir := strings.HasSuffix(name, "/")
	name = strings.TrimSuffix(name, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "://") || strings.Contains(name, "/") {
		return Entry{}, false
	}
	joined := path.Join(dir, name)
	if joined == "/" || !strings.HasPrefix(joined, strings.TrimSuffix(dir, "/")+"/") {
		return Entry{}, false
	}
	return Entry{Name: name, Path: joined, IsDir: isDir}, true
}
