// Package listwalk provides the shared recursive LIST traversal used by the
// CLI (okf export) and the MCP server (mark_index), so directory-walk
// semantics (cycle handling, entry decoding) cannot diverge per client.
package listwalk

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// Defaults for Walker's traversal bounds when the fields are zero.
const (
	DefaultMaxDepth = 16
	DefaultMaxLists = 4096
)

// ErrListBudget is returned when a walk issues more LIST requests than
// Walker.MaxLists allows; the results gathered so far are incomplete.
var ErrListBudget = errors.New("listwalk: list budget exhausted")

// Lister is the subset of the protocol client the walk needs.
type Lister interface {
	List(host, path, token string) (fetch.Result, error)
}

// Walker traverses a server's directory listings depth-first.
type Walker struct {
	Client Lister
	Host   string
	Token  string
	// Strict makes a non-OK LIST status or an invalid entry an error;
	// otherwise the directory or entry is skipped (reported via OnSkip).
	Strict bool
	// MaxDepth bounds directory nesting below root (0 = DefaultMaxDepth).
	// The seen set dedups repeated references but cannot stop a hostile
	// server minting ever-deeper paths; depth is the cycle-safety bound.
	MaxDepth int
	// MaxLists bounds total LIST requests (0 = DefaultMaxLists); exceeding
	// it returns ErrListBudget.
	MaxLists int
	// OnSkip, when set, observes lenient-mode skips (non-OK listings,
	// invalid entries, depth-capped subtrees).
	OnSkip func(path, reason string)
}

func (w *Walker) skip(skippedPath, reason string) {
	if w.OnSkip != nil {
		w.OnSkip(skippedPath, reason)
	}
}

// Entry is a resolved listing entry: a decoded, rooted document or
// directory path guaranteed to stay inside the listed directory.
type Entry struct {
	Path  string
	IsDir bool
}

// ResolveEntry resolves a raw listing link against its directory: URL-decode,
// join, and reject anything that is not a relative child (absolute paths,
// URLs, `..` escapes) — listings only ever contain relative children.
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
// root, bounded by MaxDepth and MaxLists. The seen set skips repeated
// directory references; a visit error aborts the walk.
func (w *Walker) Walk(root string, visit func(docPath string) error) error {
	if w.MaxDepth == 0 {
		w.MaxDepth = DefaultMaxDepth
	}
	if w.MaxLists == 0 {
		w.MaxLists = DefaultMaxLists
	}
	var lists int
	return w.walk(root, 0, visit, map[string]struct{}{}, &lists)
}

func (w *Walker) walk(dir string, depth int, visit func(string) error, seen map[string]struct{}, lists *int) error {
	if _, ok := seen[dir]; ok {
		return nil
	}
	seen[dir] = struct{}{}
	if depth > w.MaxDepth {
		// Depth is the cycle-safety bound: a self-referencing listing mints
		// ever-deeper distinct paths the seen set cannot catch.
		w.skip(dir, "max depth reached")
		return nil
	}
	if *lists >= w.MaxLists {
		return ErrListBudget
	}
	*lists++

	result, err := w.Client.List(w.Host, dir, w.Token)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	if result.Response.Status != protocol.StatusOK {
		if w.Strict {
			return fmt.Errorf("list %s: server returned %q", dir, result.Response.Status)
		}
		w.skip(dir, "listing returned "+result.Response.Status)
		return nil
	}

	for _, dest := range links.Extract(result.Response.Body) {
		entry, ok := ResolveEntry(dir, dest)
		if !ok {
			if w.Strict {
				return fmt.Errorf("list %s: invalid entry %q", dir, dest)
			}
			w.skip(dir, fmt.Sprintf("invalid entry %q", dest))
			continue
		}
		if entry.IsDir {
			if err := w.walk(entry.Path, depth+1, visit, seen, lists); err != nil {
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
