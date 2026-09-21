package doctor

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
)

// inventory lists the scope recursively, archived included, following every
// cursor. A truncating bound fails the audit: every check below depends on
// knowing which documents exist.
func (a *audit) inventory(ctx context.Context) error {
	var dirs []string
	docsIn := map[string][]string{}
	walker := listwalk.Walker{
		Client:          storeLister{a.store},
		IncludeArchived: true,
		MaxLists:        MaxListCalls,
		OnProblem:       a.listProblem,
		OnDir: func(dir string) {
			dirs = append(dirs, dir)
			a.dirs[dirKey(dir)] = true
		},
	}
	err := walker.Walk(ctx, a.opts.Scope, func(docPath string) error {
		if len(a.docs) >= MaxDocuments {
			return fmt.Errorf("scope holds more than %d documents", MaxDocuments)
		}
		a.docs[docPath] = &document{path: docPath}
		dir := dirKey(path.Dir(docPath))
		docsIn[dir] = append(docsIn[dir], docPath)
		return nil
	})
	switch {
	case errors.Is(err, listwalk.ErrListBudget):
		return fmt.Errorf("inventory needs more than %d list calls", MaxListCalls)
	case err != nil && ctx.Err() != nil:
		return fmt.Errorf("inventory stopped: %w", ctx.Err())
	case err != nil:
		return err
	}
	// Findings follow document order, which is breadth first by contract. The
	// walk is depth first; its preorder, stably sorted by depth, is the same.
	slices.SortStableFunc(dirs, func(x, y string) int { return depthOf(x) - depthOf(y) })
	for _, dir := range dirs {
		a.order = append(a.order, docsIn[dirKey(dir)]...)
	}
	return nil
}

// dirKey is a directory with its trailing slash, the form links name it by.
func dirKey(dir string) string { return strings.TrimSuffix(dir, "/") + "/" }

func depthOf(dir string) int { return strings.Count(dirKey(dir), "/") }

// listProblem is the audit's walk policy: an invalid entry is a coverage note,
// anything that hides part of the scope fails the audit.
func (a *audit) listProblem(p *listwalk.Problem) error {
	dir := p.Dir
	if dir != a.opts.Scope {
		dir = dirKey(dir)
	}
	switch p.Kind {
	case listwalk.ProblemInvalidEntry:
		a.note("list %s returned an invalid entry %q", dir, p.Entry)
		return nil
	case listwalk.ProblemStatus:
		if p.Status == protocol.StatusNotFound && p.Dir == a.opts.Scope {
			return fmt.Errorf("scope %s not found", dir)
		}
		return fmt.Errorf("list %s returned %s", dir, p.Status)
	case listwalk.ProblemDepth:
		return fmt.Errorf("scope nests deeper than %d directories at %s", listwalk.DefaultMaxDepth, dir)
	default:
		return fmt.Errorf("list %s: %w", dir, p.Err)
	}
}

// storeLister lets the shared walker list through the audit's Store. The
// store owns host and token, and its pages use the server's default size.
type storeLister struct{ store Store }

func (s storeLister) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	resp, err := s.store.List(ctx, r.Path, r.IncludeArchived, r.Cursor)
	return fetch.Result{Response: resp}, err
}
