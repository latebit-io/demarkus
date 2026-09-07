package retrievalbench

import (
	"context"
	"fmt"
	"time"
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

// LookupFetch is the pre-search agent behavior: one catalog lookup, then
// fetch rows in rank order until the target is in hand or MaxFetches is spent.
// An outline for the target costs one more fetch: section if named, else body.
type LookupFetch struct {
	Scope       string
	LookupLimit int
	MaxFetches  int
}

// Name implements Strategy.
func (LookupFetch) Name() string { return "lookup-fetch" }

// Run implements Strategy.
func (s LookupFetch) Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error) {
	return runLookupThenFetch(ctx, rec, q, map[string]any{
		"url": s.Scope, "query": q.Query, "limit": s.LookupLimit,
	}, s.MaxFetches)
}

// BodyFetch is the body-match behavior: one lookup with match body, then
// fetch rows in rank order, each at its anchor, until the target section is
// in hand or MaxFetches is spent. A bare-path row behaves as in LookupFetch.
type BodyFetch struct {
	Scope       string
	LookupLimit int
	MaxFetches  int
}

// Name implements Strategy.
func (BodyFetch) Name() string { return "body-fetch" }

// Run implements Strategy.
func (s BodyFetch) Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error) {
	return runLookupThenFetch(ctx, rec, q, map[string]any{
		"url": s.Scope, "query": q.Query, "limit": s.LookupLimit, "match": "body",
	}, s.MaxFetches)
}

// runLookupThenFetch is the shared shape of both strategies: rank comes from
// the lookup table, and each row is visited in order within the fetch budget.
func runLookupThenFetch(ctx context.Context, rec *Recorder, q *Question, lookupArgs map[string]any, maxFetches int) (Outcome, error) {
	lookup, err := rec.Call(ctx, "mark_lookup", lookupArgs)
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
		if fetches >= maxFetches {
			break
		}
		done, hit, err := visitRow(ctx, rec, q, row, &fetches, maxFetches)
		if err != nil {
			return Outcome{}, err
		}
		if !done {
			continue
		}
		out.Hit = hit
		if hit {
			out.CallsToEvidence = len(rec.Calls())
		}
		return out, nil
	}
	return out, nil
}

// visitRow fetches one row and reports whether the search is over: a decoy
// is not done; the target is done with its hit verdict. The named section
// must be in the fetched text; an outline costs one more fetch if affordable.
func visitRow(ctx context.Context, rec *Recorder, q *Question, row lookupRow, fetches *int, maxFetches int) (done, hit bool, err error) {
	*fetches++
	res, err := rec.Call(ctx, "mark_fetch", map[string]any{"url": row.URL()})
	if err != nil {
		return false, false, err
	}
	if row.Path != q.ExpectedPath {
		return false, false, nil
	}
	if res.IsError {
		return true, false, nil
	}
	parsed := parseFetchResponse(res.Text)
	if !parsed.isOutline() {
		return true, q.ExpectedAnchor == "" || parsed.hasSection(q.ExpectedAnchor), nil
	}
	if *fetches >= maxFetches {
		return true, false, nil
	}
	*fetches++
	args := map[string]any{"url": row.Path, "force": true}
	if q.ExpectedAnchor != "" {
		args = map[string]any{"url": row.Path + "#" + q.ExpectedAnchor}
	}
	res, err = rec.Call(ctx, "mark_fetch", args)
	if err != nil {
		return false, false, err
	}
	return true, !res.IsError, nil
}

func rankRows(rows []lookupRow, path string) int {
	for i, row := range rows {
		if row.Path == path {
			return i + 1
		}
	}
	return 0
}

// StrategyByName resolves a CLI strategy name.
func StrategyByName(name, scope string, lookupLimit, maxFetches int) (Strategy, error) {
	if lookupLimit <= 0 || maxFetches <= 0 {
		return nil, fmt.Errorf("lookup limit %d and max fetches %d must be positive", lookupLimit, maxFetches)
	}
	switch name {
	case "lookup-fetch":
		return LookupFetch{Scope: scope, LookupLimit: lookupLimit, MaxFetches: maxFetches}, nil
	case "body-fetch":
		return BodyFetch{Scope: scope, LookupLimit: lookupLimit, MaxFetches: maxFetches}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", name)
	}
}
