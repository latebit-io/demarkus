package retrievalbench

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/mdoutline"
)

// Call records one tool invocation and what it cost.
type Call struct {
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args"`
	Tokens    int            `json:"tokens"`
	ElapsedMS float64        `json:"elapsed_ms"`
	IsError   bool           `json:"is_error,omitempty"`
}

// Outcome is what a strategy achieved for one question.
type Outcome struct {
	Hit             bool
	CallsToEvidence int
	LookupRank      int
}

// Strategy is one retrieval behavior under measurement.
type Strategy interface {
	Name() string
	Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error)
}

// Recorder wraps a Tools with token counting and a call log.
type Recorder struct {
	tools   Tools
	counter TokenCounter
	calls   []Call
}

// NewRecorder starts an empty call log over tools.
func NewRecorder(tools Tools, counter TokenCounter) *Recorder {
	return &Recorder{tools: tools, counter: counter}
}

// Call invokes the tool and records tokens and elapsed time.
func (r *Recorder) Call(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	start := time.Now()
	res, err := r.tools.Call(ctx, name, args)
	elapsed := time.Since(start)
	if err != nil {
		return ToolResult{}, err
	}
	tokens, err := r.counter.Count(res.Text)
	if err != nil {
		return ToolResult{}, err
	}
	r.calls = append(r.calls, Call{
		Tool:      name,
		Args:      args,
		Tokens:    tokens,
		ElapsedMS: float64(elapsed.Microseconds()) / 1000,
		IsError:   res.IsError,
	})
	return res, nil
}

// Calls returns the log so far.
func (r *Recorder) Calls() []Call { return r.calls }

// LookupFetch is the lookup-then-fetch agent behavior: one mark_lookup (with
// match=body when Body is set), then rows in rank order until the target is
// in hand or MaxFetches is spent; an outline costs one more fetch.
type LookupFetch struct {
	Scope       string
	LookupLimit int
	MaxFetches  int
	Body        bool
}

// Name implements Strategy.
func (s LookupFetch) Name() string {
	if s.Body {
		return "body-fetch"
	}
	return "lookup-fetch"
}

// Run implements Strategy.
func (s LookupFetch) Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error) {
	args := map[string]any{"url": s.Scope, "query": q.Query, "limit": s.LookupLimit}
	if s.Body {
		args["match"] = fetch.MatchBody
	}
	lookup, err := rec.Call(ctx, "mark_lookup", args)
	if err != nil {
		return Outcome{}, err
	}
	if lookup.IsError {
		return Outcome{}, nil
	}
	rows := parseLookupRows(lookup.Text)
	out := Outcome{LookupRank: rankRows(rows, q.ExpectedPath)}
	fetches := 0
	for _, row := range rows {
		if fetches >= s.MaxFetches {
			break
		}
		visit, err := visitRow(ctx, rec, q, row, s.MaxFetches-fetches)
		if err != nil {
			return Outcome{}, err
		}
		fetches += visit.used
		if !visit.done {
			continue
		}
		out.Hit = visit.hit
		if visit.hit {
			out.CallsToEvidence = len(rec.Calls())
		}
		return out, nil
	}
	return out, nil
}

// rowVisit is the outcome of fetching one row: fetches spent, whether the
// search is over, and the verdict when it is.
type rowVisit struct {
	used int
	done bool
	hit  bool
}

// visitRow fetches one row within remaining fetches. A decoy or a sibling
// section of the target is not done, since a later row may name the right
// one. An outline costs one more fetch if affordable.
func visitRow(ctx context.Context, rec *Recorder, q *Question, row lookupRow, remaining int) (rowVisit, error) {
	res, err := rec.Call(ctx, "mark_fetch", map[string]any{"url": row.URL()})
	if err != nil {
		return rowVisit{}, err
	}
	visit := rowVisit{used: 1}
	if row.Path != q.ExpectedPath {
		return visit, nil
	}
	if res.IsError {
		visit.done = true
		return visit, nil
	}
	parsed := parseFetchResponse(res.Text)
	if !parsed.isOutline() {
		visit.hit = q.ExpectedAnchor == "" || parsed.hasSection(q.ExpectedAnchor)
		// A bare-path row was the whole document: no later row can add the
		// section. An anchored miss leaves the remaining rows to try.
		visit.done = visit.hit || row.Anchor == ""
		return visit, nil
	}
	if remaining < 2 {
		visit.done = true
		return visit, nil
	}
	args := map[string]any{"url": row.Path, "force": true}
	if q.ExpectedAnchor != "" {
		args = map[string]any{"url": row.Path + "#" + q.ExpectedAnchor}
	}
	res, err = rec.Call(ctx, "mark_fetch", args)
	if err != nil {
		return rowVisit{}, err
	}
	visit.used = 2
	visit.done = true
	visit.hit = !res.IsError
	return visit, nil
}

func rankRows(rows []lookupRow, path string) int {
	for i, row := range rows {
		if row.Path == path {
			return i + 1
		}
	}
	return 0
}

// Context is one budgeted body-match lookup: the evidence must arrive in
// the sections expanded by that single call.
type Context struct {
	Scope       string
	LookupLimit int
	Budget      int // approximate result tokens passed as the budget argument
}

// Name describes the strategy for the report header.
func (s Context) Name() string {
	return fmt.Sprintf("context (match body, limit %d, budget %d)", s.LookupLimit, s.Budget)
}

// Run issues the one call and checks the expansion for the expected section.
func (s Context) Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error) {
	args := map[string]any{"url": s.Scope, "query": q.Query, "limit": s.LookupLimit, "match": fetch.MatchBody, "budget": s.Budget}
	res, err := rec.Call(ctx, "mark_lookup", args)
	if err != nil {
		return Outcome{}, err
	}
	if res.IsError {
		return Outcome{}, nil
	}
	out := Outcome{LookupRank: rankRows(parseLookupRows(res.Text), q.ExpectedPath)}
	if expansionHolds(res.Text, q) {
		out.Hit = true
		out.CallsToEvidence = 1
	}
	return out, nil
}

// expansionHolds reports whether an expanded block carries the expected
// path and section: the anchor on its header, or a whole-document block
// whose headings include the anchor. An outline block holds nothing.
func expansionHolds(text string, q *Question) bool {
	var loc, block string
	check := func() bool {
		if loc == "" {
			return false
		}
		path, anchor := lookuptable.SplitLocation(loc)
		if path != q.ExpectedPath {
			return false
		}
		if q.ExpectedAnchor == "" || anchor == q.ExpectedAnchor {
			return true
		}
		return anchor == "" && slices.Contains(mdoutline.Anchors(block), q.ExpectedAnchor)
	}
	for line := range strings.SplitSeq(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "## "); ok && strings.HasPrefix(rest, "/") {
			if check() {
				return true
			}
			loc, block = rest, ""
			continue
		}
		if strings.HasPrefix(line, "note: ") {
			if check() {
				return true
			}
			loc, block = "", ""
			continue
		}
		block += line + "\n"
	}
	return check()
}

// StrategyByName resolves a CLI strategy name.
func StrategyByName(name, scope string, lookupLimit, maxFetches, budget int) (Strategy, error) {
	if lookupLimit <= 0 || maxFetches <= 0 {
		return nil, fmt.Errorf("lookup limit %d and max fetches %d must be positive", lookupLimit, maxFetches)
	}
	switch name {
	case "lookup-fetch", "body-fetch":
		return LookupFetch{Scope: scope, LookupLimit: lookupLimit, MaxFetches: maxFetches, Body: name == "body-fetch"}, nil
	case "context":
		if budget <= 0 {
			return nil, fmt.Errorf("budget %d must be positive", budget)
		}
		return Context{Scope: scope, LookupLimit: lookupLimit, Budget: budget}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", name)
	}
}
