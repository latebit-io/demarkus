package retrievalbench

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Config wires a run: what to measure, how to reach the endpoint, and where
// progress lines go.
type Config struct {
	Strategy Strategy
	Open     SessionOpener
	Counter  TokenCounter
	Endpoint string
	Log      io.Writer
	// Timeout bounds one question: session open plus every tool call.
	// Zero means no bound.
	Timeout time.Duration
}

// Run scores every question in set with a fresh session each. A strategy
// failure is recorded on the question as a miss so a late network fault keeps
// finished measurements; a session that cannot open aborts the run.
func Run(ctx context.Context, cfg *Config, set QuestionSet) (Report, error) {
	report := Report{
		GeneratedAt: time.Now().UTC(),
		Endpoint:    cfg.Endpoint,
		Strategy:    cfg.Strategy.Name(),
		Tokenizer:   cfg.Counter.Name(),
		Scope:       set.Scope,
		Questions:   make([]QuestionResult, 0, len(set.Questions)),
	}
	for i := range set.Questions {
		result, err := runOne(ctx, cfg, &set.Questions[i])
		if err != nil {
			return Report{}, fmt.Errorf("question %s: %w", set.Questions[i].ID, err)
		}
		report.Questions = append(report.Questions, result)
		line := fmt.Sprintf("%-4s %-10s hit=%-5v calls=%d tokens=%d %.0fms",
			result.ID, result.Category, result.Hit, result.CallsTotal, result.Tokens, result.ElapsedMS)
		if result.Error != "" {
			line += " error=" + result.Error
		}
		if err := logf(cfg.Log, "%s\n", line); err != nil {
			return Report{}, err
		}
	}
	report.Summaries = Summarize(report.Questions)
	return report, nil
}

func logf(w io.Writer, format string, args ...any) error {
	if w == nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		return fmt.Errorf("write progress: %w", err)
	}
	return nil
}

// runOne scores q in a fresh session. A teardown failure after the strategy
// finished is logged, not fatal: the measurements are already complete and
// stdio servers can exit with SIGPIPE while the client closes the pipes.
func runOne(ctx context.Context, cfg *Config, q *Question) (QuestionResult, error) {
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	session, err := cfg.Open(ctx)
	if err != nil {
		return QuestionResult{}, fmt.Errorf("open session: %w", err)
	}
	rec := NewRecorder(session, cfg.Counter)
	start := time.Now()
	outcome, runErr := cfg.Strategy.Run(ctx, rec, q)
	elapsed := time.Since(start)
	if closeErr := session.Close(); closeErr != nil {
		if err := logf(cfg.Log, "warn: %s: close session: %v\n", q.ID, closeErr); err != nil {
			return QuestionResult{}, err
		}
	}
	calls := rec.Calls()
	result := QuestionResult{
		ID:              q.ID,
		Category:        q.Category,
		Query:           q.Query,
		ExpectedPath:    q.ExpectedPath,
		ExpectedAnchor:  q.ExpectedAnchor,
		Hit:             outcome.Hit,
		CallsToEvidence: outcome.CallsToEvidence,
		CallsTotal:      len(calls),
		LookupRank:      outcome.LookupRank,
		ElapsedMS:       float64(elapsed.Microseconds()) / 1000,
		Calls:           calls,
	}
	if runErr != nil {
		result.Error = runErr.Error()
		result.Hit = false
		result.CallsToEvidence = 0
	}
	for _, c := range calls {
		result.Tokens += c.Tokens
	}
	return result, nil
}
