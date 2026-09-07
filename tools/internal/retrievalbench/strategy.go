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
// fetch rows in rank order until the target is in hand or MaxFetches is
// spent. An outline answer for the target costs one more fetch, of the
// section when the question names one and of the full body otherwise.
type LookupFetch struct {
	Scope       string
	LookupLimit int
	MaxFetches  int
}

// Name implements Strategy.
func (LookupFetch) Name() string { return "lookup-fetch" }

// Run implements Strategy.
func (s LookupFetch) Run(ctx context.Context, rec *Recorder, q *Question) (Outcome, error) {
	lookup, err := rec.Call(ctx, "mark_lookup", map[string]any{
		"url": s.Scope, "query": q.Query, "limit": s.LookupLimit,
	})
	if err != nil {
		return Outcome{}, err
	}
	if lookup.IsError {
		return Outcome{}, nil
	}
	rows := parseLookupPaths(lookup.Text)
	out := Outcome{LookupRank: rank(rows, q.ExpectedPath)}
	fetches := 0
	for _, path := range rows {
		if fetches >= s.MaxFetches {
			break
		}
		fetches++
		res, err := rec.Call(ctx, "mark_fetch", map[string]any{"url": path})
		if err != nil {
			return Outcome{}, err
		}
		if path != q.ExpectedPath {
			continue
		}
		if res.IsError {
			return out, nil
		}
		parsed := parseFetchResponse(res.Text)
		if !parsed.isOutline() {
			out.Hit = q.ExpectedAnchor == "" || parsed.hasSection(q.ExpectedAnchor)
			if out.Hit {
				out.CallsToEvidence = len(rec.Calls())
			}
			return out, nil
		}
		if fetches >= s.MaxFetches {
			return out, nil
		}
		args := map[string]any{"url": path, "force": true}
		if q.ExpectedAnchor != "" {
			args = map[string]any{"url": path + "#" + q.ExpectedAnchor}
		}
		res, err = rec.Call(ctx, "mark_fetch", args)
		if err != nil {
			return Outcome{}, err
		}
		out.Hit = !res.IsError
		if out.Hit {
			out.CallsToEvidence = len(rec.Calls())
		}
		return out, nil
	}
	return out, nil
}

func rank(rows []string, path string) int {
	for i, row := range rows {
		if row == path {
			return i + 1
		}
	}
	return 0
}

// StrategyByName resolves a CLI strategy name.
func StrategyByName(name, scope string, lookupLimit, maxFetches int) (Strategy, error) {
	switch name {
	case "lookup-fetch":
		return LookupFetch{Scope: scope, LookupLimit: lookupLimit, MaxFetches: maxFetches}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", name)
	}
}
