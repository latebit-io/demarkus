// Package listwalk provides the shared recursive LIST traversal used by the
// CLI (okf export) and the MCP server (mark_index), so directory-walk
// semantics (cycle handling, entry decoding) cannot diverge per client.
package listwalk

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// Lister is the subset of the protocol client the walk needs.
type Lister interface {
	List(host, path, token string) (fetch.Result, error)
}

// Walker traverses a server's directory listings depth-first.
type Walker struct {
	Client Lister
	Host   string
	Token  string
	// Strict makes a non-OK LIST status or an undecodable entry an error;
	// otherwise the directory or entry is skipped.
	Strict bool
}

// Entry is a resolved listing entry: a decoded, rooted document or
// directory path guaranteed to stay inside the listed directory.
type Entry struct {
	Path  string
	IsDir bool
}

// ResolveEntry resolves a raw listing link against its directory: URL-decodes
// it, joins, and rejects anything that is not a relative child (absolute
// paths, URLs, `..` escapes). Listings only ever contain relative children;
// anything else is malformed or hostile.
func ResolveEntry(dir, dest string) (Entry, bool) {
	// Listing links are URL-escaped by the server; walk decoded names.
	name, err := url.PathUnescape(dest)
	if err != nil {
		return Entry{}, false
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "://") {
		return Entry{}, false
	}
	joined := path.Join(dir, trimmed)
	// path.Join cleans ".." segments; an entry that climbed out of dir no
	// longer has it as a prefix (or collapsed to "/").
	if joined == "/" || !strings.HasPrefix(joined, strings.TrimSuffix(dir, "/")+"/") {
		return Entry{}, false
	}
	return Entry{Path: joined, IsDir: strings.HasSuffix(name, "/")}, true
}

// Walk calls visit with the decoded mark path of every file entry beneath
// root. Already-visited directories are skipped, so a listing cycle
// terminates instead of recursing forever. A visit error aborts the walk.
func (w Walker) Walk(root string, visit func(docPath string) error) error {
	return w.walk(root, visit, map[string]struct{}{})
}

func (w Walker) walk(dir string, visit func(string) error, seen map[string]struct{}) error {
	if _, ok := seen[dir]; ok {
		return nil
	}
	seen[dir] = struct{}{}

	result, err := w.Client.List(w.Host, dir, w.Token)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	if result.Response.Status != protocol.StatusOK {
		if w.Strict {
			return fmt.Errorf("list %s: server returned %q", dir, result.Response.Status)
		}
		return nil
	}

	for _, dest := range links.Extract(result.Response.Body) {
		entry, ok := ResolveEntry(dir, dest)
		if !ok {
			if w.Strict {
				return fmt.Errorf("list %s: invalid entry %q", dir, dest)
			}
			continue
		}
		if entry.IsDir {
			if err := w.walk(entry.Path, visit, seen); err != nil {
				return err
			}
			continue
		}
		if err := visit(entry.Path); err != nil {
			return err
		}
	}
	return nil
}
