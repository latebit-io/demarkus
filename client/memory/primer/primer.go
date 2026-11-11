// Package primer reads what a session starts from under one directory of a
// store: its hub, the newest journal entries, the active work and the plans.
// It reads only; what could not be read is Coverage, never an error.
package primer

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
)

// Leaf is the pseudo document under any directory that reads as its primer.
// Reserved: a write to it is refused.
const Leaf = ".context"

// IsLeaf reports the primer pseudo document.
func IsLeaf(p string) bool { return path.Base(p) == Leaf }

const (
	journalDays = 3
	bodyCap     = 4000
	planCap     = 20
	listCalls   = 50
)

// State is what the directory holds.
type State string

// States of a primed directory.
const (
	Known    State = "known"
	New      State = "new"      // no hub yet: a new project
	Archived State = "archived" // the hub is archived: a retired project
)

// Request names the directory and how to read it.
type Request struct {
	Dir    string // ends in "/"
	Layout layout.Layout
	Fetch  func(ctx context.Context, path string) (protocol.Response, error)
	// Walker lists the store; Read bounds its depth and LIST calls.
	Walker listwalk.Walker
}

// Section is one document read into the primer, cut at a line when long.
type Section struct {
	Heading  string
	Path     string
	Body     string
	Metadata map[string]string
	Cut      bool
}

// Primer is the read: the hub and newest journal entries in Sections, then
// the active work and the plans. For a New or Archived directory only Hub
// and HubPaths are set.
type Primer struct {
	Dir      string
	State    State
	Hub      string   // the archived hub's path
	HubPaths []string // where a hub may live
	Sections []Section
	// JournalHeading titles the journal; JournalEmpty: it listed no entry.
	JournalHeading string
	JournalEmpty   bool
	Active         *Section
	Plans          []string
	Coverage       []string
}

// Read renders the primer for req.Dir.
func Read(ctx context.Context, req *Request) Primer {
	p := Primer{Dir: req.Dir, State: Known, HubPaths: hub.Paths(req.Dir), JournalHeading: layout.Section(req.Layout.Journal)}
	// The hub is the first of hub.Paths the store has, live or archived.
	var hubPath, status string
	for _, hubPath = range p.HubPaths {
		var section *Section
		if section, status = p.read(ctx, req, "Hub", hubPath); section != nil {
			p.Sections = append(p.Sections, *section)
		}
		if status != protocol.StatusNotFound {
			break
		}
	}
	switch status {
	case protocol.StatusNotFound:
		return Primer{Dir: req.Dir, State: New, HubPaths: p.HubPaths}
	case protocol.StatusArchived:
		return Primer{Dir: req.Dir, State: Archived, Hub: hubPath, HubPaths: p.HubPaths}
	}
	entries, listed := p.newest(ctx, req, &listing{dir: req.Dir + req.Layout.Journal, keep: journalDays, admit: layout.IsJournalEntry})
	p.JournalEmpty = listed && len(entries) == 0
	for _, e := range entries {
		if section, _ := p.read(ctx, req, p.JournalHeading+" "+strings.TrimSuffix(path.Base(e), ".md"), e); section != nil {
			p.Sections = append(p.Sections, *section)
		}
	}
	p.Active, _ = p.read(ctx, req, "Active work", req.Dir+req.Layout.Roadmap)
	if plans, _ := p.newest(ctx, req, &listing{dir: req.Dir + req.Layout.Plans, keep: planCap}); len(plans) > 0 {
		slices.Reverse(plans)
		p.Plans = plans
	}
	return p
}

// read fetches one document and reports its status ("" on a transport
// error). An absent hub or roadmap is no gap; an absent journal entry is.
func (p *Primer) read(ctx context.Context, req *Request, heading, docPath string) (section *Section, status string) {
	optional := heading == "Hub" || heading == "Active work"
	r, err := req.Fetch(ctx, docPath)
	switch {
	case err != nil:
		p.Coverage = append(p.Coverage, fmt.Sprintf("%s: %v", docPath, err))
		return nil, ""
	case r.Status == protocol.StatusNotFound && optional:
	case r.Status != protocol.StatusOK:
		p.Coverage = append(p.Coverage, fmt.Sprintf("%s: %s", docPath, r.Status))
	default:
		kept, cut := Bound(r.Body, bodyCap)
		return &Section{Heading: heading, Path: docPath, Body: kept, Metadata: r.Metadata, Cut: cut}, r.Status
	}
	return nil, r.Status
}

// listing is one directory to list: the newest keep documents admit lets
// through (nil admits all).
type listing struct {
	dir   string
	keep  int
	admit func(name string) bool
}

// newest lists one directory and returns its newest document paths, newest
// name first; dated journal names sort by day. A missing directory is
// empty; a failed listing is not listed.
func (p *Primer) newest(ctx context.Context, req *Request, l *listing) (paths []string, listed bool) {
	w := req.Walker
	w.MaxDepth, w.MaxLists = listwalk.RootOnly, listCalls
	w.OnProblem = func(problem *listwalk.Problem) error {
		if problem.Kind == listwalk.ProblemStatus && problem.Status == protocol.StatusNotFound {
			return nil
		}
		return problem
	}
	err := w.Walk(ctx, l.dir, func(docPath string) error {
		if l.admit == nil || l.admit(path.Base(docPath)) {
			paths = append(paths, docPath)
		}
		return nil
	})
	if err != nil {
		p.Coverage = append(p.Coverage, fmt.Sprintf("%s: %v", l.dir, err))
		return nil, false
	}
	slices.Sort(paths)
	slices.Reverse(paths)
	if len(paths) > l.keep {
		paths = paths[:l.keep]
	}
	return paths, true
}

// Bound trims trailing newlines and cuts body at limit, at a line, else a
// rune boundary; cut reports whether it did.
func Bound(body string, limit int) (kept string, cut bool) {
	body = strings.TrimRight(body, "\n")
	if len(body) <= limit {
		return body, false
	}
	at := limit
	for at > 0 && body[at-1] != '\n' {
		at--
	}
	if at == 0 {
		at = limit
		for at > 0 && !utf8.RuneStart(body[at]) {
			at--
		}
	}
	return strings.TrimRight(body[:at], "\n"), true
}
