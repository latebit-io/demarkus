// Package listwalk is the shared recursive LIST traversal, so cursor following,
// cycle safety and entry decoding cannot diverge between its callers. Exported
// so tools outside this module can use it instead of writing another.
package listwalk

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listing"
	"github.com/latebit-io/demarkus/protocol"
)

// Defaults for Walker's traversal bounds when the fields are zero.
const (
	DefaultMaxDepth = 16
	DefaultMaxLists = 4096
	// RootOnly as MaxDepth lists the root directory and descends nowhere.
	RootOnly = -1
)

// ErrListBudget is returned when a walk issues more LIST requests than
// Walker.MaxLists allows; the results gathered so far are incomplete.
var ErrListBudget = errors.New("listwalk: list budget exhausted")

// ErrDepthBudget means a subtree exceeded MaxDepth and inventory is incomplete.
var ErrDepthBudget = errors.New("listwalk: depth budget exhausted")

// Lister is the subset of the protocol client the walk needs.
type Lister interface {
	List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error)
}

// ProblemKind says what went wrong with one directory or entry.
type ProblemKind int

// A walk meets four kinds of problem; each leaves the inventory incomplete.
const (
	// ProblemStatus: a LIST answered a status other than ok.
	ProblemStatus ProblemKind = iota + 1
	// ProblemInvalidEntry: an entry is not a relative child of its directory.
	ProblemInvalidEntry
	// ProblemDepth: a directory lies deeper than MaxDepth.
	ProblemDepth
	// ProblemList: a LIST failed or its page was malformed; Err holds the cause.
	ProblemList
)

// Problem is one directory or entry the walk could not take in. It is an
// error, so a policy that aborts returns it as is.
type Problem struct {
	Kind   ProblemKind
	Dir    string
	Status string // ProblemStatus
	Entry  string // ProblemInvalidEntry: the raw listing link
	Err    error  // ProblemList
}

func (p *Problem) Error() string { return fmt.Sprintf("list %s: %s", p.Dir, p.detail()) }

func (p *Problem) detail() string {
	switch p.Kind {
	case ProblemStatus:
		return fmt.Sprintf("server returned %q", p.Status)
	case ProblemInvalidEntry:
		return fmt.Sprintf("invalid entry %q", p.Entry)
	case ProblemDepth:
		return ErrDepthBudget.Error()
	default:
		return p.Err.Error()
	}
}

// Reason is the short form a caller prints next to Dir when it skips.
func (p *Problem) Reason() string {
	switch p.Kind {
	case ProblemStatus:
		return "listing returned " + p.Status
	case ProblemInvalidEntry:
		return fmt.Sprintf("invalid entry %q", p.Entry)
	case ProblemDepth:
		return "max depth reached"
	default:
		return p.Err.Error()
	}
}

// Unwrap exposes the cause, and ErrDepthBudget for a depth problem.
func (p *Problem) Unwrap() error {
	if p.Kind == ProblemDepth {
		return ErrDepthBudget
	}
	return p.Err
}

// Policy decides what a problem costs: the error it returns aborts the walk,
// nil skips that directory or entry and carries on with its siblings.
type Policy func(*Problem) error

// Abort is the policy of a nil Walker.OnProblem: the first problem ends the walk.
func Abort(p *Problem) error { return p }

// Skip carries on past a refused listing, an invalid entry and a subtree that
// is too deep, telling report (which may be nil). A failed or malformed LIST
// still aborts: what it hides is unknown, not merely unreadable.
func Skip(report func(path, reason string)) Policy {
	return func(p *Problem) error {
		if p.Kind == ProblemList {
			return p
		}
		if report != nil {
			report(p.Dir, p.Reason())
		}
		return nil
	}
}

// Walker traverses a server's directory listings depth-first.
type Walker struct {
	Client Lister
	Host   string
	Token  string
	// IncludeArchived also walks archived documents and directories.
	IncludeArchived bool
	// OnProblem is the walk's error policy; nil is Abort. Cancellation, the
	// list budget and a visit error always end the walk and never reach it.
	OnProblem Policy
	// BeforeList, when set, runs before every LIST: a crawler's politeness
	// delay. Its error ends the walk.
	BeforeList func(ctx context.Context) error
	// OnDir, when set, observes every directory the walk lists, before its
	// first LIST and in walk order. Documents go to visit, directories here.
	OnDir func(dir string)
	// MaxDepth bounds directory nesting below root (0 = DefaultMaxDepth, RootOnly = none).
	// The seen set dedups repeated references but cannot stop a hostile
	// server minting ever-deeper paths; depth is the cycle-safety bound.
	MaxDepth int
	// MaxLists bounds total LIST requests (0 = DefaultMaxLists); exceeding
	// it returns ErrListBudget.
	MaxLists int
}

// Walk calls visit with the decoded mark path of every file entry beneath
// root, bounded by MaxDepth, MaxLists and ctx. The seen set skips repeated
// directory references; a visit error aborts the walk.
func (w *Walker) Walk(ctx context.Context, root string, visit func(docPath string) error) error {
	run := walkRun{Walker: *w, ctx: ctx, visit: visit, seen: map[string]struct{}{}}
	switch {
	case run.MaxDepth == 0:
		run.MaxDepth = DefaultMaxDepth
	case run.MaxDepth < 0:
		run.MaxDepth = 0
	}
	if run.MaxLists == 0 {
		run.MaxLists = DefaultMaxLists
	}
	if run.OnProblem == nil {
		run.OnProblem = Abort
	}
	return run.walk(root, 0)
}

// walkRun is the state of one Walk, over a copy of the Walker with its
// defaults filled in, so a Walker is never changed by walking.
type walkRun struct {
	Walker
	ctx   context.Context // one Walk call; never outlives it
	visit func(docPath string) error
	seen  map[string]struct{}
	lists int
}

func (w *walkRun) walk(dir string, depth int) error {
	if _, ok := w.seen[dir]; ok {
		return nil
	}
	w.seen[dir] = struct{}{}
	// Depth is the cycle-safety bound: a self-referencing listing mints
	// ever-deeper distinct paths the seen set cannot catch.
	if depth > w.MaxDepth {
		return w.OnProblem(&Problem{Kind: ProblemDepth, Dir: dir})
	}
	if w.OnDir != nil {
		w.OnDir(dir)
	}
	pages := pageCursor{seen: make(map[string]struct{})}
	for {
		page, problem, err := w.listPage(dir, &pages)
		if err != nil {
			return err
		}
		if problem != nil {
			return w.OnProblem(problem)
		}
		for _, dest := range page.Invalid {
			if err := w.OnProblem(&Problem{Kind: ProblemInvalidEntry, Dir: dir, Entry: dest}); err != nil {
				return err
			}
		}
		for _, entry := range page.Entries {
			if entry.IsDir {
				if err := w.walk(entry.Path, depth+1); err != nil {
					return err
				}
				continue
			}
			if err := w.visit(entry.Path); err != nil {
				return err
			}
		}
		if page.Complete {
			return nil
		}
		if err := pages.advance(page.NextCursor); err != nil {
			return w.OnProblem(&Problem{Kind: ProblemList, Dir: dir, Err: err})
		}
	}
}

// pageCursor follows one directory's continuation cursors.
type pageCursor struct {
	cursor, lastName string
	seen             map[string]struct{}
}

// advance refuses a cursor that repeats: a server that does not move would
// otherwise hold the walk until the list budget runs out.
func (p *pageCursor) advance(next string) error {
	if _, duplicate := p.seen[next]; duplicate || next == p.cursor {
		return errors.New("continuation cursor did not advance")
	}
	p.seen[next] = struct{}{}
	p.cursor = next
	return nil
}

// listPage reads the next page of dir. The error ends the walk; the problem
// goes to the policy.
func (w *walkRun) listPage(dir string, pages *pageCursor) (listing.Page, *Problem, error) {
	if err := w.ctx.Err(); err != nil {
		return listing.Page{}, nil, err
	}
	if w.lists >= w.MaxLists {
		return listing.Page{}, nil, ErrListBudget
	}
	w.lists++
	if w.BeforeList != nil {
		if err := w.BeforeList(w.ctx); err != nil {
			return listing.Page{}, nil, err
		}
	}
	result, err := w.Client.List(w.ctx, fetch.ListRequest{
		Host: w.Host, Path: dir, Token: w.Token,
		IncludeArchived: w.IncludeArchived,
		Cursor:          pages.cursor,
		PageSize:        protocol.MaxListPageSize,
	})
	if err != nil {
		// A cancelled LIST is the walk ending, not a directory to skip.
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			return listing.Page{}, nil, ctxErr
		}
		return listing.Page{}, &Problem{Kind: ProblemList, Dir: dir, Err: err}, nil
	}
	if result.Response.Status != protocol.StatusOK {
		return listing.Page{}, &Problem{Kind: ProblemStatus, Dir: dir, Status: result.Response.Status}, nil
	}
	page, err := listing.ParsePage(dir, result.Response, pages.lastName)
	if err != nil {
		return listing.Page{}, &Problem{Kind: ProblemList, Dir: dir, Err: err}, nil
	}
	pages.lastName = page.LastName
	return page, nil, nil
}
